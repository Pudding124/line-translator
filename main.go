package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/oauth2/google"
)

const maxInputCharacters = 300
const inputTooLongReply = "原文超過 300 字，請分段傳送。 / Teks melebihi 300 karakter. Silakan kirim dalam beberapa bagian."

type event struct {
	Type       string `json:"type"`
	ReplyToken string `json:"replyToken"`
	Mode       string `json:"mode"`
	Source     struct {
		Type string `json:"type"`
	} `json:"source"`
	Message struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"message"`
}

type service struct {
	secret       string
	token        string
	parent       string
	google       *http.Client
	line         *http.Client
	translateURL string
	replyURL     string
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	secret, token, project := os.Getenv("LINE_CHANNEL_SECRET"), os.Getenv("LINE_CHANNEL_ACCESS_TOKEN"), os.Getenv("GOOGLE_CLOUD_PROJECT")
	if secret == "" || token == "" || project == "" {
		return fmt.Errorf("LINE_CHANNEL_SECRET, LINE_CHANNEL_ACCESS_TOKEN and GOOGLE_CLOUD_PROJECT are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-translation")
	if err != nil {
		return fmt.Errorf("Google credentials: %w", err)
	}
	client.Timeout = 10 * time.Second
	s := &service{secret: secret, token: token, parent: "projects/" + project + "/locations/global", google: client, line: &http.Client{Timeout: 10 * time.Second}, translateURL: "https://translation.googleapis.com/v3/", replyURL: "https://api.line.me/v2/bot/message/reply"}
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.webhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("listening on :%s", port)
	select {
	case err := <-done:
		if err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func (s *service) webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid or oversized body", 413)
		return
	}
	signature, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Line-Signature"))
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write(body)
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		http.Error(w, "invalid signature", 401)
		return
	}
	var payload struct {
		Events []event `json:"events"`
	}
	if json.Unmarshal(body, &payload) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	// Finish API calls before acknowledging: Cloud Run may suspend CPU after response.
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, e := range payload.Events {
		if e.Type != "message" || e.Message.Type != "text" || e.Source.Type != "group" || e.ReplyToken == "" || e.Mode == "standby" || strings.TrimSpace(e.Message.Text) == "" {
			continue
		}
		wg.Add(1)
		go func(e event) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			if utf8.RuneCountInString(e.Message.Text) > maxInputCharacters {
				if err := s.reply(ctx, e.ReplyToken, inputTooLongReply); err != nil {
					log.Printf("length notice reply failed: %v", err)
				}
				return
			}
			translated, err := s.translate(ctx, e.Message.Text)
			if err != nil {
				log.Printf("translation failed: %v", err)
				return
			}
			if translated == "" {
				return
			}
			if err := s.reply(ctx, e.ReplyToken, translated); err != nil {
				log.Printf("reply failed: %v", err)
			}
		}(e)
	}
	wg.Wait()
	// Do not request redelivery of a batch whose reply tokens may already be used.
	w.WriteHeader(http.StatusOK)
}

func postJSON(ctx context.Context, client *http.Client, endpoint, token string, body, output interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API returned status %d", resp.StatusCode)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output)
}

func targetLanguage(source string) string {
	switch strings.ToLower(source) {
	case "id":
		return "zh-TW"
	case "zh", "zh-cn", "zh-tw", "zh-hans", "zh-hant":
		return "id"
	default:
		return ""
	}
}

func (s *service) translate(ctx context.Context, text string) (string, error) {
	var detected struct {
		Languages []struct {
			LanguageCode string `json:"languageCode"`
		} `json:"languages"`
	}
	base := s.translateURL + "projects/" + url.PathEscape(strings.TrimPrefix(strings.TrimSuffix(s.parent, "/locations/global"), "projects/")) + "/locations/global"
	err := postJSON(ctx, s.google, base+":detectLanguage", "", map[string]interface{}{"content": text, "mimeType": "text/plain"}, &detected)
	if err != nil {
		return "", err
	}
	if len(detected.Languages) == 0 {
		return "", fmt.Errorf("no detected language")
	}
	source := detected.Languages[0].LanguageCode
	target := targetLanguage(source)
	if target == "" {
		return "", nil
	}
	var result struct {
		Translations []struct {
			Text string `json:"translatedText"`
		} `json:"translations"`
	}
	err = postJSON(ctx, s.google, base+":translateText", "", map[string]interface{}{"contents": []string{text}, "mimeType": "text/plain", "sourceLanguageCode": source, "targetLanguageCode": target}, &result)
	if err != nil {
		return "", err
	}
	if len(result.Translations) == 0 {
		return "", fmt.Errorf("empty translation")
	}
	return result.Translations[0].Text, nil
}

func (s *service) reply(ctx context.Context, token, text string) error {
	// LINE limits text to 5000 UTF-16 units and replies to five messages.
	chunks := splitText(text, 5000)
	if len(chunks) > 5 {
		chunks = chunks[:5]
		chunks[4] = "翻譯結果過長，請將原文分段傳送。 / Hasil terlalu panjang; kirim teks dalam beberapa bagian."
	}
	messages := make([]map[string]string, 0, len(chunks))
	for _, chunk := range chunks {
		messages = append(messages, map[string]string{"type": "text", "text": chunk})
	}
	return postJSON(ctx, s.line, s.replyURL, s.token, map[string]interface{}{"replyToken": token, "messages": messages}, nil)
}

func splitText(text string, limit int) []string {
	var result []string
	start, units := 0, 0
	for i, r := range text {
		size := 1
		if utf16.IsSurrogate(r) || r > 0xFFFF {
			size = 2
		}
		if units+size > limit {
			result = append(result, text[start:i])
			start = i
			units = 0
		}
		units += size
	}
	if start < len(text) {
		result = append(result, text[start:])
	}
	return result
}

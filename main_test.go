package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestWebhookInputLength(t *testing.T) {
	for _, tc := range []struct {
		name            string
		text            string
		wantGoogleCalls int
	}{
		{"300 Chinese characters", strings.Repeat("中", 300), 2},
		{"301 Chinese characters", strings.Repeat("中", 301), 0},
		{"300 emoji", strings.Repeat("😀", 300), 2},
		{"301 emoji", strings.Repeat("😀", 301), 0},
		{"301 ASCII characters", strings.Repeat("a", 301), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			googleCalls, replies := 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/reply" {
					replies++
					var body struct {
						Messages []struct {
							Text string `json:"text"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					want := "translated"
					if tc.wantGoogleCalls == 0 {
						want = inputTooLongReply
					}
					if len(body.Messages) != 1 || body.Messages[0].Text != want {
						t.Errorf("unexpected reply: %+v", body)
					}
					return
				}
				googleCalls++
				if strings.HasSuffix(r.URL.Path, ":detectLanguage") {
					w.Write([]byte(`{"languages":[{"languageCode":"zh"}]}`))
				} else {
					w.Write([]byte(`{"translations":[{"translatedText":"translated"}]}`))
				}
			}))
			defer api.Close()
			s := &service{secret: "secret", parent: "projects/test/locations/global", google: api.Client(), line: api.Client(), translateURL: api.URL + "/", replyURL: api.URL + "/reply"}
			e := event{Type: "message", ReplyToken: "token"}
			e.Source.Type = "group"
			e.Message.Type, e.Message.Text = "text", tc.text
			body, err := json.Marshal(map[string]interface{}{"events": []event{e}})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(body)))
			mac := hmac.New(sha256.New, []byte("secret"))
			mac.Write(body)
			req.Header.Set("X-Line-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			w := httptest.NewRecorder()
			s.webhook(w, req)
			if w.Code != 200 || googleCalls != tc.wantGoogleCalls || replies != 1 {
				t.Fatalf("status=%d Google calls=%d (want %d) replies=%d", w.Code, googleCalls, tc.wantGoogleCalls, replies)
			}
		})
	}
}

func TestWebhookTranslation(t *testing.T) {
	for _, tc := range []struct{ source, target string }{{"id", "zh-TW"}, {"zh-CN", "id"}, {"en", ""}} {
		t.Run(tc.source, func(t *testing.T) {
			detects, translations, replies := 0, 0, 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, ":detectLanguage"):
					detects++
					json.NewEncoder(w).Encode(map[string]interface{}{"languages": []map[string]string{{"languageCode": tc.source}}})
				case strings.HasSuffix(r.URL.Path, ":translateText"):
					translations++
					var body struct {
						Target string `json:"targetLanguageCode"`
						Source string `json:"sourceLanguageCode"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					if body.Target != tc.target || body.Source != tc.source {
						t.Errorf("unexpected translation request: %+v", body)
					}
					w.Write([]byte(`{"translations":[{"translatedText":"翻譯結果"}]}`))
				case r.URL.Path == "/reply":
					replies++
					if r.Header.Get("Authorization") != "Bearer access" {
						t.Error("missing access token")
					}
					var body struct {
						Token    string `json:"replyToken"`
						Messages []struct {
							Text string `json:"text"`
						} `json:"messages"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					if body.Token != "reply-token" || len(body.Messages) != 1 || body.Messages[0].Text != "翻譯結果" {
						t.Errorf("unexpected reply: %+v", body)
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer api.Close()
			s := &service{secret: "secret", token: "access", parent: "projects/test/locations/global", google: api.Client(), line: api.Client(), translateURL: api.URL + "/", replyURL: api.URL + "/reply"}
			body := `{"events":[{"type":"message","replyToken":"reply-token","source":{"type":"group"},"message":{"type":"text","text":"hello"}}]}`
			req := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
			mac := hmac.New(sha256.New, []byte("secret"))
			mac.Write([]byte(body))
			req.Header.Set("X-Line-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			response := httptest.NewRecorder()
			s.webhook(response, req)
			expected := 1
			if tc.target == "" {
				expected = 0
			}
			if response.Code != 200 || detects != 1 || translations != expected || replies != expected {
				t.Fatalf("status=%d detects=%d translations=%d replies=%d", response.Code, detects, translations, replies)
			}
		})
	}
}

func TestWebhookValidation(t *testing.T) {
	s := &service{secret: "secret"}
	for _, tc := range []struct {
		body, method string
		signed       bool
		status       int
	}{
		{`{"events":[]}`, "POST", false, 401},
		{`{"events":[]}`, "POST", true, 200},
		{`invalid`, "POST", true, 400},
		{``, "GET", false, 405},
		{`{"events":[{"type":"message","replyToken":"x","source":{"type":"user"},"message":{"type":"text","text":"hi"}}]}`, "POST", true, 200},
	} {
		req := httptest.NewRequest(tc.method, "/webhook", strings.NewReader(tc.body))
		if tc.signed {
			mac := hmac.New(sha256.New, []byte("secret"))
			mac.Write([]byte(tc.body))
			req.Header.Set("X-Line-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		s.webhook(w, req)
		if w.Code != tc.status {
			t.Errorf("got %d want %d", w.Code, tc.status)
		}
	}
}

func TestSplitText(t *testing.T) {
	original := strings.Repeat("😀", 2501) + "中文"
	chunks := splitText(original, 5000)
	if len(chunks) != 2 || strings.Join(chunks, "") != original {
		t.Fatal("text was lost or split incorrectly")
	}
	for _, chunk := range chunks {
		if len(utf16.Encode([]rune(chunk))) > 5000 {
			t.Fatal("chunk exceeds LINE limit")
		}
	}
}

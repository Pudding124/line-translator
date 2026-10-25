# LINE 印尼文／中文翻譯 Bot

Go webhook，部署於 Cloud Run，使用 Google Cloud Translation Advanced v3 與 LINE Messaging API。

## 行為

- 原文最多 300 個 Unicode 字元（含空白、標點與換行）；剛好 300 字仍翻譯。超過時不呼叫 Google 語言偵測或翻譯 API，僅回覆中印雙語分段提示。組合 emoji 可能包含多個 Unicode 字元。

- `POST /webhook`：先以原始 body 驗證 LINE HMAC-SHA256 簽章。
- 僅處理群組文字訊息；印尼文 → 繁體中文（`zh-TW`），繁簡中文 → 印尼文（`id`）。其他語言、私人訊息、貼圖與 standby event 略過。
- Google 自動偵測語言，再翻譯；每則支援語言的訊息會呼叫 Google 兩次。短句、混合語言可能誤判，依主要偵測語言決定方向。
- 使用該事件的 reply token 回覆原群組，不需 group ID，也不使用 push API。
- 長結果以 UTF-16 單位切分，最多五則；超出時以第五則提醒分段傳送。
- `GET /healthz`：健康檢查。

## 本機執行（PowerShell）

安裝 Go 與 Google Cloud CLI。本機可用 Go 1.18+，部署映像使用 Go 1.26。

```powershell
gcloud auth application-default login
$env:GOOGLE_CLOUD_PROJECT = "YOUR_PROJECT_ID"
gcloud auth application-default set-quota-project $env:GOOGLE_CLOUD_PROJECT
$env:LINE_CHANNEL_SECRET = "YOUR_CHANNEL_SECRET"
$env:LINE_CHANNEL_ACCESS_TOKEN = "YOUR_CHANNEL_ACCESS_TOKEN"
go run .
```

本機帳號需有該專案的 Cloud Translation API User 權限與配額專案使用權限；專案需啟用計費與 Cloud Translation API。`.env.example` 僅為變數清單，程式不會自動讀取 `.env`。

```powershell
go test ./...
go vet ./...
```

## GitHub Actions：僅建置並推送 image

`.github/workflows/push-image.yml` 在 push 到 `main` 或手動執行時，使用 Dockerfile 建置 `linux/amd64` image，推送至 Google Artifact Registry。workflow 不部署 Cloud Run，也不需要 LINE secrets。

workflow 已固定使用區域 `asia-east1`、專案 `mars-personal-512604`、repository `line-translator`。

在 GitHub repository 的 **Settings → Secrets and variables → Actions → Secrets → New repository secret** 設定：

| Secret | 值或範例 |
| --- | --- |
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | `projects/123456789/locations/global/workloadIdentityPools/github/providers/github` |
| `GCP_SERVICE_ACCOUNT` | `github-actions@mars-personal-512604.iam.gserviceaccount.com` |

Google Cloud 需事先建立 Docker 格式的 Artifact Registry repository，並啟用 Artifact Registry、IAM Service Account Credentials 與 Security Token Service API。設定 GitHub OIDC 的 Workload Identity Federation，將 provider 的 attribute condition 限制為你的 GitHub repository；若只允許主分支推送，再限制 `assertion.ref == 'refs/heads/main'`，手動執行也請選 `main`。

授予該 repository 對應的 federated principal 在上述服務帳號上的 `roles/iam.workloadIdentityUser`，並授予服務帳號在目的 Artifact Registry repository 上的 `roles/artifactregistry.writer`。此帳號只負責推 image，不需要 Cloud Run 部署權限，也不需上傳服務帳號 JSON 金鑰。驗證設定可參考 [Google auth action 文件](https://github.com/google-github-actions/auth#workload-identity-federation-through-a-service-account)。

image 路徑為 `asia-east1-docker.pkg.dev/mars-personal-512604/line-translator/webhook:sha-COMMIT_SHA`。每次成功後，Actions summary 會列出 tag 與含 digest 的完整路徑，可自行用於 Cloud Run 部署。workflow 使用完整 commit SHA 作為 tag，不推送 `latest`；重跑同一 commit 仍可能更新該 tag，需要固定版本時請使用 digest。

以下保留手動部署指令；若已透過 GitHub Actions 推送 image，可略過 `gcloud builds submit`，並將 `$image` 換成 Actions summary 的 image 路徑。

## 部署至 Cloud Run（PowerShell）

以下命令會建立雲端資源並產生費用，請自行替換專案、區域與名稱。執行帳號需有啟用 API、管理 IAM、Secret Manager、Artifact Registry、Cloud Build 與部署 Cloud Run 的權限。

```powershell
$project = "YOUR_PROJECT_ID"
$region = "asia-east1"
$serviceAccount = "line-translator@$project.iam.gserviceaccount.com"
gcloud config set project $project
gcloud services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com translate.googleapis.com secretmanager.googleapis.com
gcloud iam service-accounts create line-translator --display-name="LINE Translator"
gcloud projects add-iam-policy-binding $project --member="serviceAccount:$serviceAccount" --role="roles/cloudtranslate.user"
gcloud artifacts repositories create line-translator --repository-format=docker --location=$region
```

在 Secret Manager 建立 `line-channel-secret` 與 `line-channel-access-token`，分別填入 LINE channel secret 與 channel access token。可用 Cloud Console 建立，避免將秘密寫入 shell 歷史。再授權服務帳號讀取這兩個 secret：

```powershell
gcloud secrets add-iam-policy-binding line-channel-secret --member="serviceAccount:$serviceAccount" --role="roles/secretmanager.secretAccessor"
gcloud secrets add-iam-policy-binding line-channel-access-token --member="serviceAccount:$serviceAccount" --role="roles/secretmanager.secretAccessor"
$image = "$region-docker.pkg.dev/$project/line-translator/webhook:v1"
gcloud builds submit --tag=$image .
gcloud run deploy line-translator --image=$image --region=$region --service-account=$serviceAccount --allow-unauthenticated --port=8080 --timeout=60s --concurrency=20 --max-instances=3 --set-env-vars="GOOGLE_CLOUD_PROJECT=$project" --set-secrets="LINE_CHANNEL_SECRET=line-channel-secret:latest,LINE_CHANNEL_ACCESS_TOKEN=line-channel-access-token:latest"
gcloud run services describe line-translator --region=$region --format="value(status.url)"
```

Cloud Build 的建置服務帳號需要有目的 Artifact Registry repository 的寫入權限。Cloud Run 使用執行服務帳號取得 ADC，不需 JSON 金鑰。Webhook 必須公開讓 LINE 存取，以 LINE 簽章作為驗證。

## LINE 設定

1. 建立 LINE Official Account 並啟用 Messaging API，取得上述 channel secret 與 channel access token。
2. 開啟 **Allow bot to join group chats**。
3. Webhook URL 設為 `https://YOUR_CLOUD_RUN_URL/webhook`，按 Verify，啟用 **Use webhook**。
4. 關閉官方帳號預設的自動回覆，避免額外訊息。
5. 將 Bot 邀入群組，分別輸入 `你好，今天好嗎？` 與 `Selamat pagi, apa kabar?` 測試。

## 錯誤處理與限制

每次 webhook 最多處理 25 秒，同時處理最多八個事件；API client 各有 10 秒 timeout。回覆前完成處理，避免 Cloud Run 在 HTTP 回應後暫停 CPU。

API 失敗會記錄狀態碼，不記錄訊息、reply token 或憑證。合法 webhook 處理後回傳 200，即使翻譯／回覆失敗亦不重送，避免批次內已成功回覆的事件重複執行。因此暫時性故障會漏訊息。本版沒有跨執行個體持久去重，LINE 重送可能重複呼叫翻譯 API；reply token 只能使用一次。若需要可靠重試，需加入以 webhookEventId 為鍵的持久狀態，且重試必須在 token 有效時間內完成。

這是可部署的初版。高流量時請依 API 配額、翻譯費用與群組使用量調整 concurrency/max-instances；目前沒有群組允許清單或個別群組用量限制。

## 官方文件

- [LINE 簽章驗證](https://developers.line.biz/en/docs/messaging-api/verify-webhook-signature/)
- [LINE Reply API 與限制](https://developers.line.biz/en/reference/messaging-api/#send-reply-message)
- [Google Translation Advanced](https://docs.cloud.google.com/translate/docs/translate-text)
- [Google IAM 權限](https://docs.cloud.google.com/translate/docs/migrate-to-v3)

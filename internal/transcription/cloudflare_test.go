package transcription

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

func TestCloudflareAITranscriber(t *testing.T) {
	// Get Cloudflare credentials from environment
	accountID := os.Getenv("CF_ACCOUNT_ID")
	apiKey := os.Getenv("CF_API_KEY")
	if accountID == "" || apiKey == "" {
		t.Skip("Skipping test: CF_ACCOUNT_ID and CF_API_KEY environment variables not set")
	}

	// Create test logger
	logger := zaptest.NewLogger(t).Sugar().Desugar()

	// Test cases
	testCases := []struct {
		name  string
		model string
	}{
		{
			name:  "Nova3",
			model: "@cf/deepgram/nova-3",
		},
		{
			name:  "WhisperLargeV3Turbo",
			model: "@cf/openai/whisper-large-v3-turbo",
		},
	}

	// Audio file to test
	audioFile := "/home/ubuntu/whatsapp_transcriber_go_gus/new1.opus"

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create transcriber
			transcriber := NewCloudflareAITranscriber(accountID, apiKey, tc.model, logger)

			// Transcribe audio
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			transcript, err := transcriber.TranscribeAudio(ctx, audioFile, "")
			if err != nil {
				t.Fatalf("Transcription failed: %v", err)
			}

			// Verify and parse transcript
			if transcript == "" {
				t.Error("Empty transcript received")
			} else {
				// Parse the transcript response
				var result struct {
					Result struct {
						TranscriptionInfo struct {
							Language string `json:"language"`
						} `json:"transcription_info"`
						Text string `json:"text"`
					} `json:"result"`
				}

				if err := json.Unmarshal([]byte(transcript), &result); err != nil {
					t.Errorf("Failed to parse transcript: %v", err)
				} else {
					cleanText := strings.TrimSpace(result.Result.Text)
					preview := cleanText
					if len(preview) > 100 {
						preview = preview[:100] + "..."
					}
					t.Logf("Detected language: %s", result.Result.TranscriptionInfo.Language)
					t.Logf("Clean transcript (%d chars): %s", len(cleanText), preview)
				}
			}
		})
	}
}

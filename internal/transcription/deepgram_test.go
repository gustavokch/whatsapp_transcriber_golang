package transcription

import (
	"context"
	"os"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

func TestDeepgramAITranscriber(t *testing.T) {
	// Get Deepgram API key from environment
	apiKey := os.Getenv("DEEPGRAM_API_KEY")
	if apiKey == "" {
		t.Skip("Skipping test: DEEPGRAM_API_KEY environment variable not set")
	}

	// Create test logger
	logger := zaptest.NewLogger(t).Sugar().Desugar()

	// Audio file to test
	audioFile := "/home/ubuntu/whatsapp_transcriber_go_gus/new1.opus"

	// Create transcriber
	transcriber := NewDeepgramAITranscriber(apiKey, logger)

	// Transcribe audio
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	transcript, err := transcriber.TranscribeAudio(ctx, audioFile, "")
	if err != nil {
		t.Fatalf("Transcription failed: %v", err)
	}

	// Verify transcript
	if transcript == "" {
		t.Error("Empty transcript received")
	} else {
		t.Logf("Transcript (%d chars): %s", len(transcript), truncateString(transcript, 100))
	}
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

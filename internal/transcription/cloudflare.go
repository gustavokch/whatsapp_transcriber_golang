package transcription

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// CloudflareAITranscriber implements the Transcriber interface for Cloudflare AI.
type CloudflareAITranscriber struct {
	AccountID string
	APIKey    string
	Model     string
	Logger    *zap.Logger
}

// NewCloudflareAITranscriber creates a new CloudflareAITranscriber.
func NewCloudflareAITranscriber(accountID, apiKey, model string, logger *zap.Logger) *CloudflareAITranscriber {
	return &CloudflareAITranscriber{
		AccountID: accountID,
		APIKey:    apiKey,
		Model:     model,
		Logger:    logger,
	}
}

// TranscribeAudio sends an audio file to Cloudflare AI for transcription.
func (c *CloudflareAITranscriber) TranscribeAudio(ctx context.Context, audioFilePath string, language string) (string, error) {
	audioBytes, err := os.ReadFile(audioFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read audio file: %w", err)
	}

	apiURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/run/%s", c.AccountID, c.Model)
	c.Logger.Debug("Calling Cloudflare AI API", zap.String("url", apiURL))

	// Different models have different API requirements
	// Whisper models expect JSON format with audio as base64
	// Deepgram models expect audio as array
	if strings.Contains(c.Model, "whisper") {
		return c.transcribeWithWhisperAPI(ctx, apiURL, audioBytes, language)
	} else {
		return c.transcribeWithDeepgramAPI(ctx, apiURL, audioBytes, audioFilePath)
	}
}

// transcribeWithWhisperAPI uses the Whisper API format (JSON with base64 audio)
func (c *CloudflareAITranscriber) transcribeWithWhisperAPI(ctx context.Context, apiURL string, audioBytes []byte, language string) (string, error) {
	// Encode audio to base64 for Whisper API
	base64Audio := base64.StdEncoding.EncodeToString(audioBytes)

	// Construct request body according to Whisper API schema
	requestBody := map[string]interface{}{
		"audio":      base64Audio,
		"task":       "transcribe", // default task
		"vad_filter": false,
	}

	// Add optional parameters
	if language != "" {
		requestBody["language"] = language
	}
	// Note: initial_prompt and prefix could be added here if available

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Whisper request: %w", err)
	}

	// Add detailed debug logging
	truncatedBody := string(jsonBody)
	if len(truncatedBody) > 200 {
		truncatedBody = truncatedBody[:200] + "..."
	}
	c.Logger.Debug("Whisper API request details",
		zap.String("url", apiURL),
		zap.Int("original_audio_size", len(audioBytes)),
		zap.Int("base64_size", len(base64Audio)),
		zap.String("content_type", "application/json"),
		zap.String("request_body_truncated", truncatedBody),
		zap.String("model", c.Model))

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	client := &http.Client{Timeout: 60 * time.Second} // 60 seconds timeout for transcription
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Cloudflare AI: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("cloudflare ai whisper api returned non-200 status: %d, body: %s", resp.StatusCode, string(respBody))
	}

	// Read full response body first since we can only read it once
	bodyBytes, _ := io.ReadAll(resp.Body)

	// Try parsing into full result structure first
	var fullResult struct {
		Text              string `json:"text"`
		WordCount         int    `json:"word_count"`
		TranscriptionInfo struct {
			Language            string  `json:"language"`
			LanguageProbability float64 `json:"language_probability"`
			Duration            float64 `json:"duration"`
			DurationAfterVAD    float64 `json:"duration_after_vad"`
		} `json:"transcription_info"`
		Segments []struct {
			Start            float64 `json:"start"`
			End              float64 `json:"end"`
			Text             string  `json:"text"`
			Temperature      float64 `json:"temperature"`
			AvgLogprob       float64 `json:"avg_logprob"`
			CompressionRatio float64 `json:"compression_ratio"`
			NoSpeechProb     float64 `json:"no_speech_prob"`
			Words            []struct {
				Word  string  `json:"word"`
				Start float64 `json:"start"`
				End   float64 `json:"end"`
			} `json:"words"`
		} `json:"segments"`
		VTT string `json:"vtt"`
	}

	if err := json.Unmarshal(bodyBytes, &fullResult); err == nil && fullResult.Text != "" {
		// Prepare segment preview
		segmentPreview := ""
		if len(fullResult.Segments) > 0 {
			segmentText := fullResult.Segments[0].Text
			if len(segmentText) > 50 {
				segmentText = segmentText[:50] + "..."
			}
			segmentPreview = fmt.Sprintf("%d segments, first: %q", len(fullResult.Segments), segmentText)
		}

		// Prepare VTT preview
		vttPreview := ""
		if fullResult.VTT != "" {
			vttPreview = fullResult.VTT
			if len(vttPreview) > 50 {
				vttPreview = vttPreview[:50] + "..."
			}
		}

		c.Logger.Debug("Whisper API response details",
			zap.String("transcription", fullResult.Text),
			zap.Int("word_count", fullResult.WordCount),
			zap.String("detected_language", fullResult.TranscriptionInfo.Language),
			zap.Float64("language_probability", fullResult.TranscriptionInfo.LanguageProbability),
			zap.Float64("duration", fullResult.TranscriptionInfo.Duration),
			zap.Float64("duration_after_vad", fullResult.TranscriptionInfo.DurationAfterVAD),
			zap.String("segments_preview", segmentPreview),
			zap.String("vtt_preview", vttPreview))

		return fullResult.Text, nil
	}

	// Fallback to simple text extraction
	var simpleResult struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(bodyBytes, &simpleResult); err == nil && simpleResult.Text != "" {
		return simpleResult.Text, nil
	}

	// Final fallback to raw text
	if text := string(bodyBytes); text != "" {
		return text, nil
	}

	return "", fmt.Errorf("empty transcription received from Whisper API")
}

// transcribeWithDeepgramAPI uses the Deepgram API format with strict schema compliance
func (c *CloudflareAITranscriber) transcribeWithDeepgramAPI(ctx context.Context, apiURL string, audioBytes []byte, audioFilePath string) (string, error) {
	contentType := getContentType(audioFilePath)
	base64Audio := base64.StdEncoding.EncodeToString(audioBytes)

	// Create request structure matching Deepgram API schema
	type AudioPayload struct {
		Body        string `json:"body"`
		ContentType string `json:"contentType"`
	}

	request := struct {
		Audio            AudioPayload `json:"audio"`
		CustomTopicMode  string       `json:"custom_topic_mode,omitempty"`
		CustomTopic      string       `json:"custom_topic,omitempty"`
		CustomIntentMode string       `json:"custom_intent_mode,omitempty"`
		CustomIntent     string       `json:"custom_intent,omitempty"`
		DetectEntities   bool         `json:"detect_entities,omitempty"`
		DetectLanguage   bool         `json:"detect_language,omitempty"`
		Diarize          bool         `json:"diarize,omitempty"`
		Dictation        bool         `json:"dictation,omitempty"`
		Encoding         string       `json:"encoding,omitempty"`
		Extra            string       `json:"extra,omitempty"`
		FilterWords      bool         `json:"filter_words,omitempty"`
		Keyterm          string       `json:"keyterm,omitempty"`
		Keywords         string       `json:"keywords,omitempty"`
		Language         string       `json:"language,omitempty"`
		Measurements     bool         `json:"measurements,omitempty"`
		MipOptOut        bool         `json:"mip_opt_out,omitempty"`
		Mode             string       `json:"mode,omitempty"`
		Multichannel     bool         `json:"multichannel,omitempty"`
		Numerals         bool         `json:"numerals,omitempty"`
		Paragraphs       bool         `json:"paragraphs,omitempty"`
		ProfanityFilter  bool         `json:"profanity_filter,omitempty"`
		Punctuate        bool         `json:"punctuate,omitempty"`
		Redact           string       `json:"redact,omitempty"`
		Replace          string       `json:"replace,omitempty"`
		Search           string       `json:"search,omitempty"`
		Sentiment        bool         `json:"sentiment,omitempty"`
		SmartFormat      bool         `json:"smart_format,omitempty"`
		Topics           bool         `json:"topics,omitempty"`
		Utterances       bool         `json:"utterances,omitempty"`
		UttSplit         float64      `json:"utt_split,omitempty"`
	}{
		Audio: AudioPayload{
			Body:        base64Audio,
			ContentType: contentType,
		},
		DetectLanguage: true, // Default parameter we're currently using
	}

	requestBody, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Deepgram request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	// Log the request details
	truncatedBody := string(requestBody)
	if len(truncatedBody) > 200 {
		truncatedBody = truncatedBody[:200] + "..."
	}
	c.Logger.Debug("Deepgram API request details",
		zap.String("url", apiURL),
		zap.Int("original_audio_size", len(audioBytes)),
		zap.Int("base64_size", len(base64Audio)),
		zap.String("content_type", contentType),
		zap.String("request_body_truncated", truncatedBody),
		zap.String("model", c.Model))

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Cloudflare AI: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		c.Logger.Error("Deepgram API error response",
			zap.Int("status_code", resp.StatusCode),
			zap.String("response_body", string(respBody)))
		return "", fmt.Errorf("cloudflare ai deepgram api returned non-200 status: %d, body: %s", resp.StatusCode, string(respBody))
	}

	bodyBytes, _ := io.ReadAll(resp.Body)

	// Define response structure matching Deepgram output schema
	var apiResponse struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
					Words      []struct {
						Word       string  `json:"word"`
						Start      float64 `json:"start"`
						End        float64 `json:"end"`
						Confidence float64 `json:"confidence"`
					} `json:"words"`
					Confidence float64 `json:"confidence"`
				} `json:"alternatives"`
			} `json:"channels"`
			Summary struct {
				Result string `json:"result"`
				Short  string `json:"short"`
			} `json:"summary"`
			Sentiments struct {
				Segments []struct {
					Text           string  `json:"text"`
					StartWord      int     `json:"start_word"`
					EndWord        int     `json:"end_word"`
					Sentiment      string  `json:"sentiment"`
					SentimentScore float64 `json:"sentiment_score"`
				} `json:"segments"`
				Average struct {
					Sentiment      string  `json:"sentiment"`
					SentimentScore float64 `json:"sentiment_score"`
				} `json:"average"`
			} `json:"sentiments"`
		} `json:"results"`
	}

	if err := json.Unmarshal(bodyBytes, &apiResponse); err != nil {
		c.Logger.Warn("Failed to parse Deepgram JSON response", zap.Error(err))
		// Fallback to simple text extraction
		if transcript := string(bodyBytes); transcript != "" {
			return transcript, nil
		}
		return "", fmt.Errorf("failed to parse Deepgram response: %w", err)
	}

	// Extract transcript from structured response
	var transcript string
	if len(apiResponse.Results.Channels) > 0 &&
		len(apiResponse.Results.Channels[0].Alternatives) > 0 {
		transcript = apiResponse.Results.Channels[0].Alternatives[0].Transcript
	}

	if transcript == "" {
		// Try summary as fallback
		if apiResponse.Results.Summary.Result != "" {
			transcript = apiResponse.Results.Summary.Result
		} else if apiResponse.Results.Summary.Short != "" {
			transcript = apiResponse.Results.Summary.Short
		}
	}

	if transcript == "" {
		c.Logger.Error("Empty transcript in Deepgram response",
			zap.Any("full_response", apiResponse))
		return "", fmt.Errorf("empty transcript received from Deepgram API")
	}

	// Log response details
	c.Logger.Debug("Deepgram API response details",
		zap.String("transcript", transcript[:min(50, len(transcript))]),
		zap.Int("channels", len(apiResponse.Results.Channels)),
		zap.Int("alternatives", len(apiResponse.Results.Channels[0].Alternatives)),
		zap.Int("words", len(apiResponse.Results.Channels[0].Alternatives[0].Words)),
		zap.String("summary", apiResponse.Results.Summary.Short),
		zap.Int("sentiment_segments", len(apiResponse.Results.Sentiments.Segments)))

	return transcript, nil
}

package ai

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

// GeminiClient wraps the Google Generative AI SDK for structured
// financial analysis prompts. It manages the client lifecycle,
// model configuration, and retry logic.
type GeminiClient struct {
	client *genai.Client
	model  *genai.GenerativeModel
	apiKey string
}

// NewGeminiClient creates a Gemini client with the given API key.
func NewGeminiClient(ctx context.Context, apiKey string) (*GeminiClient, error) {
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}

	model := client.GenerativeModel("gemini-3.1-flash-lite-preview")

	// Configure for financial analysis
	model.SetTemperature(0.3) // low creativity, high precision
	model.SetTopP(0.8)
	model.SetTopK(40)
	model.SetMaxOutputTokens(4096)

	// Safety settings: allow financial discussion
	model.SafetySettings = []*genai.SafetySetting{
		{Category: genai.HarmCategoryHarassment, Threshold: genai.HarmBlockOnlyHigh},
		{Category: genai.HarmCategoryDangerousContent, Threshold: genai.HarmBlockOnlyHigh},
	}

	return &GeminiClient{
		client: client,
		model:  model,
		apiKey: apiKey,
	}, nil
}

// Generate sends a prompt and returns the text response.
// Includes retry logic with exponential backoff for transient errors.
func (gc *GeminiClient) Generate(ctx context.Context, prompt string) (string, error) {
	var lastErr error

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			log.Printf("[gemini] retry %d after %v", attempt, backoff)
			time.Sleep(backoff)
		}

		resp, err := gc.model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			lastErr = fmt.Errorf("generate (attempt %d): %w", attempt+1, err)
			// Retry on transient errors
			if isTransient(err) {
				continue
			}
			return "", lastErr
		}

		text := extractText(resp)
		if text == "" {
			lastErr = fmt.Errorf("empty response (attempt %d)", attempt+1)
			continue
		}

		return text, nil
	}

	return "", fmt.Errorf("all retries exhausted: %w", lastErr)
}

// GenerateWithSystem sends a prompt with a system instruction.
func (gc *GeminiClient) GenerateWithSystem(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	// Clone model config with system instruction
	model := gc.client.GenerativeModel("gemini-3.1-flash-lite-preview")
	model.SetTemperature(0.3)
	model.SetTopP(0.8)
	model.SetTopK(40)
	model.SetMaxOutputTokens(4096)
	model.SystemInstruction = genai.NewUserContent(genai.Text(systemPrompt))

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * time.Second)
		}

		resp, err := model.GenerateContent(ctx, genai.Text(userPrompt))
		if err != nil {
			lastErr = err
			if isTransient(err) {
				continue
			}
			return "", fmt.Errorf("generate with system: %w", err)
		}

		text := extractText(resp)
		if text != "" {
			return text, nil
		}
		lastErr = fmt.Errorf("empty response")
	}

	return "", fmt.Errorf("all retries exhausted: %w", lastErr)
}

// Close releases the Gemini client resources.
func (gc *GeminiClient) Close() {
	if gc.client != nil {
		gc.client.Close()
	}
}

// --- helpers ---

// extractText pulls the text content from a Gemini response.
func extractText(resp *genai.GenerateContentResponse) string {
	if resp == nil || len(resp.Candidates) == 0 {
		return ""
	}

	candidate := resp.Candidates[0]
	if candidate.Content == nil || len(candidate.Content.Parts) == 0 {
		return ""
	}

	var parts []string
	for _, part := range candidate.Content.Parts {
		if textPart, ok := part.(genai.Text); ok {
			parts = append(parts, string(textPart))
		}
	}

	return strings.Join(parts, "")
}

// isTransient checks if an error is likely transient (worth retrying).
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "429") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "500") ||
		strings.Contains(errStr, "deadline") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "unavailable")
}

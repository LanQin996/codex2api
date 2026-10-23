package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
)

func TestNormalizeExternalImageJobInputsTotalBudget(t *testing.T) {
	img := "data:image/png;base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 40))
	req := &imageGenerationJobPayload{InputImages: []string{img, img}}
	if err := normalizeExternalImageJobInputsWithLimit(context.Background(), req, 150); err == nil {
		t.Fatal("expected total input budget error")
	}
	req = &imageGenerationJobPayload{InputImages: []string{img, img}}
	if err := normalizeExternalImageJobInputsWithLimit(context.Background(), req, 300); err != nil {
		t.Fatalf("unexpected error under budget: %v", err)
	}
}

func TestNormalizeExternalImageJobFieldsRejectsOversizedSize(t *testing.T) {
	for _, size := range []string{"20000x20000", "16385x1", "8000x8000", "999999999999999999999999x1"} {
		for _, upscale := range []string{"", "none", "4k"} {
			req := &imageGenerationJobPayload{Prompt: "test", Size: size, Upscale: upscale}
			if _, err := normalizeExternalImageJobFields(req); err == nil {
				t.Errorf("oversized size %q accepted with upscale %q", size, upscale)
			}
		}
	}
	for _, size := range []string{"1920x1080", "16384x1", "auto", "4k", ""} {
		req := &imageGenerationJobPayload{Prompt: "test", Size: size}
		if _, err := normalizeExternalImageJobFields(req); err != nil {
			t.Errorf("valid size %q rejected: %v", size, err)
		}
	}
}

// Command llmgate-probe calls a Provider directly so the upstream contract
// can be exercised from a shell, CI cron, or interactive debugging — without
// the HTTP server in the loop.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"

	"llmgate/internal/config"
	"llmgate/internal/provider"
	"llmgate/internal/provider/opencode"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	_ = godotenv.Load()

	model := flag.String("model", "", "model id (defaults to LLMGATE_DEFAULT_MODEL)")
	prompt := flag.String("prompt", "", "user prompt; if omitted, reads OpenAI request JSON from stdin")
	maxTokens := flag.Int("max-tokens", 128, "max_tokens for the request")
	rawOut := flag.Bool("raw", false, "print only the assistant text (no JSON envelope)")
	streamOut := flag.Bool("stream", false, "stream tokens as server-sent events arrive")
	flag.Parse()

	cfg, err := config.LoadProvider()
	if err != nil {
		return err
	}

	req, err := buildRequest(cfg, *model, *prompt, *maxTokens)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := opencode.New(cfg.OpenCodeAPIKey, opencode.WithBaseURL(cfg.OpenCodeBaseURL))
	if *streamOut {
		return runStream(ctx, p, req)
	}

	resp, err := p.Complete(ctx, req)
	if err != nil {
		return err
	}

	if *rawOut {
		if len(resp.Choices) == 0 {
			return errors.New("upstream returned no choices")
		}
		fmt.Println(resp.Choices[0].Message.Content)
		return nil
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(resp)
}

func runStream(ctx context.Context, p provider.Provider, req *provider.Request) error {
	stream, err := p.CompleteStream(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()

	out := bufio.NewWriter(os.Stdout)
	var reasoning strings.Builder
	var finishReason string
	chunks := 0
	contentChunks := 0
	reasoningChunks := 0

	for {
		event, err := stream.Recv()
		if errors.Is(err, provider.ErrStreamDone) {
			break
		}
		if err != nil {
			return err
		}

		chunks++
		if len(event.Choices) == 0 {
			continue
		}
		choice := event.Choices[0]
		if choice.Delta.Content != "" {
			contentChunks++
			if _, err := fmt.Fprint(out, choice.Delta.Content); err != nil {
				return err
			}
			if err := out.Flush(); err != nil {
				return err
			}
		}
		if choice.Delta.ReasoningContent != "" {
			reasoningChunks++
			reasoning.WriteString(choice.Delta.ReasoningContent)
		}
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
	}

	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	if reasoning.Len() > 0 {
		if _, err := fmt.Fprintf(out, "\nreasoning (truncated to 400 chars): %s\n", truncateRunes(reasoning.String(), 400)); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out,
		"chunks: %d\nchunks w/ content: %d\nchunks w/ reasoning: %d\nfinish_reason: %s\n",
		chunks, contentChunks, reasoningChunks, finishReason,
	)
	if err != nil {
		return err
	}
	return out.Flush()
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

func buildRequest(cfg *config.Provider, model, prompt string, maxTokens int) (*provider.Request, error) {
	if prompt != "" {
		m := model
		if m == "" {
			m = cfg.DefaultModel
		}
		return &provider.Request{
			Model:     m,
			Messages:  []provider.Message{{Role: "user", Content: prompt}},
			MaxTokens: maxTokens,
		}, nil
	}

	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("no -prompt given and stdin is empty")
	}
	req := &provider.Request{}
	if err := json.Unmarshal(body, req); err != nil {
		return nil, fmt.Errorf("decode stdin as OpenAI request: %w", err)
	}
	if req.Model == "" {
		if model != "" {
			req.Model = model
		} else {
			req.Model = cfg.DefaultModel
		}
	}
	return req, nil
}

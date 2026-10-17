package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/tts"
)

func TestResolveMediaPath(t *testing.T) {
	tmpDir := os.TempDir()

	// Create a temp workspace with a test file for workspace-relative tests.
	workspace := t.TempDir()
	docsDir := filepath.Join(workspace, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(docsDir, "report.pdf")
	if err := os.WriteFile(testFile, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("restricted", func(t *testing.T) {
		tool := NewMessageTool(workspace, true)
		ctx := context.Background()

		tests := []struct {
			name   string
			input  string
			want   string
			wantOK bool
		}{
			// Temp paths outside the workspace are not a separate trust boundary.
			{"outside temp file", "MEDIA:" + filepath.Join(tmpDir, "test.png"), "", false},
			{"outside nested temp", "MEDIA:" + filepath.Join(tmpDir, "sub", "file.txt"), "", false},

			// Workspace files allowed
			{"workspace absolute", "MEDIA:" + testFile, testFile, true},
			{"workspace relative", "MEDIA:docs/report.pdf", testFile, true},

			// Not a MEDIA: message
			{"no prefix", filepath.Join(tmpDir, "test.png"), "", false},
			{"empty after prefix", "MEDIA:", "", false},
			{"dot path", "MEDIA:.", "", false},
			{"empty string", "", "", false},
			{"just MEDIA", "MEDIA", "", false},

			// Outside workspace paths are blocked, including siblings under /tmp/.
			{"outside workspace", "MEDIA:/etc/passwd", "", false},
			{"traversal attack", "MEDIA:" + filepath.Join(workspace, "..", "etc", "passwd"), "", false},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, ok := tool.resolveMediaPath(ctx, tt.input)
				if ok != tt.wantOK {
					t.Errorf("resolveMediaPath(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
				}
				if ok && got != tt.want {
					t.Errorf("resolveMediaPath(%q) = %q, want %q", tt.input, got, tt.want)
				}
			})
		}
	})

	t.Run("unrestricted", func(t *testing.T) {
		tool := NewMessageTool(workspace, false)
		ctx := context.Background()

		tests := []struct {
			name   string
			input  string
			wantOK bool
		}{
			{"any absolute path", "MEDIA:/etc/hostname", true},
			{"workspace relative", "MEDIA:docs/report.pdf", true},
			{"temp file", "MEDIA:" + filepath.Join(tmpDir, "test.png"), true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, ok := tool.resolveMediaPath(ctx, tt.input)
				if ok != tt.wantOK {
					t.Errorf("resolveMediaPath(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
				}
			})
		}
	})

	t.Run("context workspace override", func(t *testing.T) {
		// Tool has no workspace, but context provides one.
		tool := NewMessageTool("", true)
		ctx := WithToolWorkspace(context.Background(), workspace)

		got, ok := tool.resolveMediaPath(ctx, "MEDIA:docs/report.pdf")
		if !ok {
			t.Fatal("expected ok=true for workspace-relative path with context workspace")
		}
		if got != testFile {
			t.Errorf("got %q, want %q", got, testFile)
		}
	})
}

func TestMessageMediaWorkspaceBoundary(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(workspace, "report.pdf")
	outside := filepath.Join(root, "secret.txt")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(workspace, "linked.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		path      string
		wantMedia bool
	}{
		{"relative workspace file", "report.pdf", true},
		{"absolute workspace file", inside, true},
		{"absolute temp sibling", outside, false},
		{"relative traversal", "../secret.txt", false},
		{"absolute traversal", workspace + "/../secret.txt", false},
		{"symlink escape", link, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgBus := bus.New()
			tool := NewMessageTool("", true)
			tool.SetMessageBus(msgBus)
			ctx := WithToolWorkspace(context.Background(), workspace)
			message := "MEDIA:" + tc.path
			result := tool.Execute(ctx, map[string]any{
				"action": "send", "channel": "telegram", "target": "chat", "message": message,
			})
			if result.IsError {
				t.Fatalf("Execute: %s", result.ForLLM)
			}
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			out, ok := msgBus.SubscribeOutbound(ctx)
			if !ok {
				t.Fatal("missing outbound message")
			}
			if tc.wantMedia {
				if len(out.Media) != 1 || out.Media[0].URL != inside {
					t.Fatalf("workspace attachment = %+v, want %q", out.Media, inside)
				}
			} else if len(out.Media) != 0 || out.Content != message {
				t.Fatalf("escaped path published as attachment: %+v", out)
			}
		})
	}
}

func TestTtsMediaWorkspaceBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			http.NotFound(w, r)
			return
		}
		if _, err := w.Write([]byte("audio")); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	manager := tts.NewManager(tts.ManagerConfig{})
	manager.RegisterProvider(tts.NewOpenAIProvider(tts.OpenAIConfig{APIBase: server.URL}))
	for _, restricted := range []bool{true, false} {
		t.Run(fmt.Sprintf("restricted=%v", restricted), func(t *testing.T) {
			ctx := context.Background()
			if restricted {
				ctx = WithToolWorkspace(ctx, t.TempDir())
			}
			result := NewTtsTool(manager).Execute(ctx, map[string]any{"text": "hello"})
			if result.IsError {
				t.Fatal(result.ForLLM)
			}
			tool := NewMessageTool("", restricted)
			msgBus := bus.New()
			tool.SetMessageBus(msgBus)
			sent := tool.Execute(ctx, map[string]any{
				"action": "send", "channel": "telegram", "target": "chat", "message": result.ForLLM,
			})
			if sent.IsError {
				t.Fatal(sent.ForLLM)
			}
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			out, ok := msgBus.SubscribeOutbound(ctx)
			if !ok || len(out.Media) != 1 {
				t.Fatalf("generated TTS media not attached: %+v", out)
			}
			path := out.Media[0].URL
			if !restricted {
				t.Cleanup(func() {
					if err := os.Remove(path); err != nil {
						t.Error(err)
					}
				})
			}
			if audio, err := os.ReadFile(path); err != nil || string(audio) != "audio" {
				t.Fatalf("generated audio = %q, err = %v", audio, err)
			}
		})
	}
}

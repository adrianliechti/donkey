package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type requestTransport func(*http.Request) (*http.Response, error)

func (f requestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelCallUsesCallerCancellationWithoutRequestDeadline(t *testing.T) {
	o := newOpenAI("fixture", "test-model")
	client := &http.Client{Transport: requestTransport(func(r *http.Request) (*http.Response, error) {
		if deadline, ok := r.Context().Deadline(); ok {
			t.Errorf("model request gained an unexpected deadline: %s", deadline)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"response","output":[]}`)), Request: r}, nil
	})}
	_, err := o.client.Responses.New(context.Background(), responses.ResponseNewParams{Model: "test-model"}, option.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
}

func TestStructuredRequestUsesStrictProjectSchema(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text struct {
				Format struct {
					Type   string
					Name   string
					Strict bool
					Schema json.RawMessage
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		format := request.Text.Format
		var want, got any
		data, _ := json.Marshal(projectSchema)
		json.Unmarshal(data, &want)
		json.Unmarshal(format.Schema, &got)
		if format.Type != "json_schema" || format.Name != "project" || !format.Strict || !reflect.DeepEqual(got, want) {
			t.Error("request lost the strict project schema")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Reading the apps"}]},{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"{\"apps\":[],\"connections\":[]}"}]}]}`))
	}))
	defer provider.Close()
	o := &openAI{client: openai.NewClient(option.WithBaseURL(provider.URL), option.WithAPIKey("test")), model: "test"}
	var out Project
	if err := o.structured(context.Background(), "Read the app", "repository", "project", projectSchema, &out); err != nil {
		t.Fatal(err)
	}
	if out.Apps == nil || out.Connections == nil {
		t.Fatal("final answer wasn't decoded")
	}
}

func TestAnswerTextSeparatesFinalOutput(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		wantErr         bool
	}{
		{"unphased", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`, `{}`, false},
		{"legacy provider without status", `{"output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`, `{}`, false},
		{"two JSON messages", `{"status":"completed","output":[{"type":"message","phase":"commentary","content":[{"type":"output_text","text":"{\"draft\":true}"}]},{"type":"message","phase":"final_answer","content":[{"type":"output_text","text":"{\"apps\":[]}"}]}]}`, `{"apps":[]}`, false},
		{"commentary only", `{"status":"completed","output":[{"type":"message","phase":"commentary","content":[{"type":"output_text","text":"{}"}]}]}`, "", true},
		{"incomplete JSON", `{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`, "", true},
		{"refusal", `{"status":"completed","output":[{"type":"message","phase":"final_answer","content":[{"type":"refusal","refusal":"No"}]}]}`, "", true},
		{"ambiguous messages", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{}"}]},{"type":"message","content":[{"type":"output_text","text":"{}"}]}]}`, "", true},
		{"split JSON blocks", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{"},{"type":"output_text","text":"}"}]}]}`, "", true},
		{"no answer", `{"status":"completed","output":[]}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response responses.Response
			if err := json.Unmarshal([]byte(tc.raw), &response); err != nil {
				t.Fatal(err)
			}
			got, err := answerText(&response)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestDockerfileToolsEditAndRetainTranscriptOnAnswerRetry(t *testing.T) {
	root := write(t, map[string]string{"server.js": "listen(3000)\n"})
	calls, answers, imageChecks := 0, 0, 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input json.RawMessage `json:"input"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if strings.Contains(string(request.Input), "runtime-secret-value") {
			t.Error("secret sent to model")
		}
		output := []any{}
		reply := func(text string) any {
			return map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
		}
		if len(request.Tools) == 0 {
			answers++
			if !strings.Contains(string(request.Input), "Edited server.js") || !strings.Contains(string(request.Input), "opaque-test-reasoning") {
				t.Error("answer lost edit/ reasoning transcript")
			}
			if !strings.Contains(string(request.Input), "inspected-digest") {
				t.Error("answer lost live base-image evidence")
			}
			if answers == 1 {
				output = append(output, reply("invalid JSON"))
			} else {
				output = append(output, reply(`{"dockerfile":"FROM node:22\nCMD [\"node\",\"server.js\"]\n","dockerignore":"","notes":"Fixed the listener"}`))
			}
		} else {
			names := []string{}
			for _, tool := range request.Tools {
				names = append(names, tool.Name)
			}
			if !reflect.DeepEqual(names, []string{"read", "glob", "grep", "edit", "check_image"}) {
				t.Errorf("wrong tool set: %v", names)
			}
			switch calls {
			case 0:
				output = append(output, map[string]any{"id": "reasoning-1", "type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-test-reasoning"})
				output = append(output, map[string]any{"type": "function_call", "call_id": "read-1", "name": "read", "arguments": `{"file_path":"server.js","offset":1,"limit":1}`})
				output = append(output, map[string]any{"type": "function_call", "call_id": "image-1", "name": "check_image", "arguments": `{"image":"node:22-alpine"}`})
			case 1:
				if !strings.Contains(string(request.Input), "listen(3000)") {
					t.Error("missing read result")
				}
				output = append(output, map[string]any{"type": "function_call", "call_id": "edit-1", "name": "edit", "arguments": `{"edits":[{"file_path":"server.js","old_string":"listen(3000)","new_string":"listen(8080)","replace_all":false}]}`})
			default:
				output = append(output, reply("The source now uses the reviewed port."))
			}
			calls++
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": output})
	}))
	defer provider.Close()
	o := &openAI{client: openai.NewClient(option.WithBaseURL(provider.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)), model: "test"}
	result, err := o.WriteDockerfile(context.Background(), &DockerfileInput{Root: root, Port: 8080, AllowEdits: true, KnownSecrets: []string{"runtime-secret-value"}, Attempts: []DockerfileAttempt{{Stage: "scan", Failure: "raw runtime-secret-value"}},
		CheckImage: func(ctx context.Context, image string) (string, error) {
			imageChecks++
			if image != "node:22-alpine" {
				t.Errorf("wrong image lookup: %s", image)
			}
			return "inspected-digest; runtime-secret-value", nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || answers != 2 || imageChecks != 1 || len(result.Changes) != 1 || len(result.FilesRead) != 1 {
		t.Fatalf("wrong investigation result: %+v, calls=%d answers=%d", result, calls, answers)
	}
	data, err := os.ReadFile(filepath.Join(root, "server.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "listen(8080)\n" {
		t.Fatalf("edit not applied: %s", data)
	}
}

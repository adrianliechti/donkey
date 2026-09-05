package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

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

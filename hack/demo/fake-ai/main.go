// Command fake-ai is a deterministic stand-in for an OpenAI-compatible
// endpoint — DEMO/TEST tooling, like hack/demo/fake-idp. donkey's assistant
// speaks the Responses API and parses the answer as JSON against a schema;
// this server always answers one message whose text is "{}", which reads as
// "no findings" everywhere: detection keeps what donkey worked out itself,
// diagnosis stays empty, and deploys run without a real model or a key.
//
// Run: go run ./hack/demo/fake-ai   (listens on :8080, override with PORT)
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp_fake",
			"object":     "response",
			"created_at": 0,
			"status":     "completed",
			"model":      "fake-ai",
			"output": []any{
				map[string]any{
					"type":   "message",
					"id":     "msg_fake",
					"role":   "assistant",
					"status": "completed",
					"content": []any{
						map[string]any{"type": "output_text", "text": "{}", "annotations": []any{}},
					},
				},
			},
		})
	})
	log.Printf("fake-ai answering on :%s (every question gets {})", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

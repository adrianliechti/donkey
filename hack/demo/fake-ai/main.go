// Command fake-ai is a deterministic stand-in for an OpenAI-compatible
// endpoint — DEMO/TEST tooling, like hack/demo/fake-idp. donkey's assistant
// speaks the Responses API and parses the answer as JSON against a schema.
//
// Almost every question gets "{}", which reads as "no findings" everywhere:
// detection keeps what donkey worked out itself, diagnosis stays empty, and
// deploys run without a real model or a key.
//
// The exception is the Dockerfile donkey's own builder asks for, because "no
// findings" is not an answer there — `build.builder: ai` would have nothing
// to build. So this server reads the file tree out of the prompt and answers
// with a Dockerfile for the stack it recognizes. It is a stand-in for a
// model, not a detector: it covers the shapes donkey's own testdata uses, and
// says so plainly when it does not recognize one.
//
// Run: go run ./hack/demo/fake-ai   (listens on :8080, override with PORT)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response(answer(body)))
	})
	log.Printf("fake-ai answering on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// answer picks the reply for one request: the Dockerfile schema gets a
// Dockerfile, everything else gets "no findings".
func answer(body []byte) string {
	var req struct {
		Text struct {
			Format struct {
				Name string `json:"name"`
			} `json:"format"`
		} `json:"text"`
	}
	_ = json.Unmarshal(body, &req)
	if req.Text.Format.Name != "dockerfile" {
		return "{}"
	}
	out, _ := json.Marshal(map[string]string{
		"dockerfile": dockerfileFor(string(body), portOf(string(body))),
		"notes":      "fake-ai: a fixed Dockerfile for the detected stack",
	})
	return string(out)
}

var portRE = regexp.MustCompile(`donkey will set PORT=(\d+)`)

func portOf(prompt string) string {
	if m := portRE.FindStringSubmatch(prompt); m != nil {
		return m[1]
	}
	return "8080"
}

// dockerfileFor recognizes the stacks donkey's samples are written in. The
// order matters: a Node project with a Python tool in it is still Node.
func dockerfileFor(prompt, port string) string {
	has := func(s string) bool { return strings.Contains(prompt, s) }
	switch {
	case has("package.json"):
		return fmt.Sprintf(`FROM node:22-alpine
WORKDIR /app
COPY package*.json ./
RUN npm install --omit=dev || npm install
COPY . .
ENV PORT=%s
CMD ["node", "index.js"]
`, port)
	case has("requirements.txt"):
		return fmt.Sprintf(`FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
ENV PORT=%s
CMD ["sh", "-c", "python app.py"]
`, port)
	case has("go.mod"):
		return `FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN go build -o /out/app ./...

FROM alpine:3.20
COPY --from=build /out/app /app
CMD ["/app"]
`
	case has("index.html"):
		// nginx-unprivileged, because donkey drops every capability: stock
		// nginx chowns its cache directory on startup and dies.
		return fmt.Sprintf(`FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY --chown=101:101 . /usr/share/nginx/html
EXPOSE %s
`, port)
	}
	// Saying so is better than a Dockerfile that cannot work: donkey reports
	// it, and a real model would have answered.
	return "# fake-ai does not recognize this stack\nFROM alpine:3.20\nRUN echo 'fake-ai has no Dockerfile for this app' >&2 && exit 1\n"
}

func response(text string) map[string]any {
	return map[string]any{
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
					map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
				},
			},
		},
	}
}

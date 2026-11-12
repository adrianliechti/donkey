package detect

import "testing"

func TestStackReadsTheBaseImages(t *testing.T) {
	for _, tc := range []struct {
		images []string
		want   string
	}{
		{[]string{"python:3.12-slim"}, "python"},
		{[]string{"docker.io/library/node:22-alpine", "nginx:1.27"}, "node"},
		{[]string{"golang:1.24", "gcr.io/distroless/static-debian12"}, "go"},
		{[]string{"maven:3.9-eclipse-temurin-21", "eclipse-temurin:21-jre"}, "java"},
		{[]string{"gcr.io/distroless/java21-debian12"}, "java"},
		{[]string{"mcr.microsoft.com/dotnet/sdk:9.0", "mcr.microsoft.com/dotnet/aspnet:9.0"}, "dotnet"},
		{[]string{"oven/bun:1"}, "bun"},
		{[]string{"ghcr.io/astral-sh/uv:python3.12-bookworm"}, "python"},
		{[]string{"caddy:2-alpine"}, StackStatic},
		{[]string{"registry.example.com:5000/node@sha256:abc"}, "node"},
		{[]string{"ubuntu:24.04"}, ""},
		{nil, ""},
	} {
		if got := Stack(tc.images); got != tc.want {
			t.Errorf("Stack(%q) = %q, want %q", tc.images, got, tc.want)
		}
	}
}

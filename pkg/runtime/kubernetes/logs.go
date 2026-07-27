package kubernetes

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

// StreamLogs writes the logs of every pod of the selected component to out,
// prefixed with the pod name when there is more than one.
func (c *Client) StreamLogs(ctx context.Context, namespace, app string, opts rt.LogOptions, out io.Writer) error {
	selector := fmt.Sprintf("donkey.dev/app=%s,donkey.dev/component=%s", app, opts.Component)
	pods, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		if strings.HasPrefix(opts.Component, "addon-") {
			return fmt.Errorf("no running copies found — is the service still in the space? `donkey addon list` shows them")
		}
		return fmt.Errorf("no running copies found — is the app deployed?")
	}

	plo := &corev1.PodLogOptions{Follow: opts.Follow, TailLines: &opts.TailLines}
	if opts.Since > 0 {
		secs := int64(opts.Since.Seconds())
		plo.SinceSeconds = &secs
	}

	prefix := len(pods.Items) > 1
	var wg sync.WaitGroup
	var mu sync.Mutex
	started := 0
	var streamErr error
	for _, pod := range pods.Items {
		rc, err := c.Clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, plo).Stream(ctx)
		if err != nil {
			// A pod that is still starting has no logs yet — say so instead
			// of silently printing nothing. Under the same lock as the
			// streaming goroutines: by this point an earlier pod may already
			// be writing, and out is an http.ResponseWriter.
			mu.Lock()
			fmt.Fprintf(out, "[%s] no logs yet (%s)\n", pod.Name, podSummary(&pod))
			mu.Unlock()
			streamErr = err
			continue
		}
		started++
		wg.Add(1)
		go func(name string, rc io.ReadCloser) {
			defer wg.Done()
			defer rc.Close()
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				mu.Lock()
				if prefix {
					fmt.Fprintf(out, "[%s] %s\n", name, sc.Text())
				} else {
					fmt.Fprintln(out, sc.Text())
				}
				mu.Unlock()
			}
		}(pod.Name, rc)
	}
	wg.Wait()
	if started == 0 && streamErr != nil {
		return fmt.Errorf("couldn't read logs from any pod: %w", streamErr)
	}
	return nil
}

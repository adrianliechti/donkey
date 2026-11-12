package packages

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// Streaming keeps memory proportional to the largest scalar, rather than
// every historical version of a package. The pipe also provides backpressure
// when the package manager reads slowly or cancels its request.
func streamNPMMetadata(upstream io.ReadCloser, request proxyRequest) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer upstream.Close()
		decoder := json.NewDecoder(upstream)
		decoder.UseNumber()
		buffer := bufio.NewWriter(writer)
		err := rewriteNPMValue(decoder, buffer, "", request)
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			if _, trailing := decoder.Token(); trailing != io.EOF {
				if trailing == nil {
					trailing = fmt.Errorf("multiple values in npm metadata")
				}
				err = trailing
			}
		}
		if err == nil {
			err = buffer.Flush()
		}
		_ = writer.CloseWithError(err)
	}()
	return &npmMetadataBody{PipeReader: reader, upstream: upstream}
}

type npmMetadataBody struct {
	*io.PipeReader
	upstream io.ReadCloser
}

func (b *npmMetadataBody) Close() error {
	// Unblock both a pending upstream read and a pending downstream write.
	_ = b.PipeReader.Close()
	return b.upstream.Close()
}

func rewriteNPMValue(decoder *json.Decoder, out *bufio.Writer, key string, request proxyRequest) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); ok {
		if delimiter != '{' && delimiter != '[' {
			return fmt.Errorf("unexpected npm metadata delimiter %q", delimiter)
		}
		if err := out.WriteByte(byte(delimiter)); err != nil {
			return err
		}
		first := true
		for decoder.More() {
			if !first {
				if err := out.WriteByte(','); err != nil {
					return err
				}
			}
			first = false
			childKey := key
			if delimiter == '{' {
				name, err := decoder.Token()
				if err != nil {
					return err
				}
				var ok bool
				childKey, ok = name.(string)
				if !ok {
					return fmt.Errorf("npm metadata object key is not a string")
				}
				encoded, _ := json.Marshal(childKey)
				if _, err := out.Write(encoded); err != nil {
					return err
				}
				if err := out.WriteByte(':'); err != nil {
					return err
				}
			}
			if err := rewriteNPMValue(decoder, out, childKey, request); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		return out.WriteByte(byte(end.(json.Delim)))
	}
	value := any(token)
	rewriteJSONURLs(&value, key, request)
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = out.Write(encoded)
	return err
}

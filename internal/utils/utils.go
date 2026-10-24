// Package utils hosts small helper routines shared across commands.
package utils

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// maxConfirmAttempts is the maximum number of confirmation prompt retries.
	maxConfirmAttempts = 3
	// Size constants for byte formatting.
	kb = 1024
	mb = 1024 * kb
	gb = 1024 * mb
)

// AskConfirm prompts on out and returns true if the user confirms on in.
// Reuse one in across calls: a new reader loses buffered input.
// ctx must be non-nil.
func AskConfirm(ctx context.Context, in *bufio.Reader, out io.Writer, prompt string) (bool, error) {
	message := strings.TrimSpace(prompt)
	if message == "" {
		message = "Proceed?"
	}

	_, _ = fmt.Fprintln(out)
	for i := range maxConfirmAttempts {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		_, _ = fmt.Fprintf(out, "%s [y/N]: ", message)
		response, err := readLine(ctx, in)
		// Piped input such as `printf y |` may end without a newline; a
		// final non-empty answer still counts.
		if err != nil && (!errors.Is(err, io.EOF) || strings.TrimSpace(response) == "") {
			return false, fmt.Errorf("error reading user input: %w", err)
		}

		response = strings.ToLower(strings.TrimSpace(response))
		switch response {
		case "yes", "y":
			return true, nil
		case "no", "n", "":
			return false, nil
		default:
			if i < maxConfirmAttempts-1 {
				_, _ = fmt.Fprintln(out, "Please answer with 'yes'/'no' or 'y'/'n'.")
			}
		}
	}

	return false, nil
}

func readLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	type result struct {
		err  error
		line string
	}

	resultCh := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		resultCh <- result{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-resultCh:
		return res.line, res.err
	}
}

// FormatSize converts bytes to a human-readable string (B, KB, MB, GB).
func FormatSize(bytes uint64) string {
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

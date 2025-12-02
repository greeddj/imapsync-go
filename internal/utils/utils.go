// Package utils hosts small helper routines shared across commands.
package utils

import (
	"bufio"
	"fmt"
	"os"
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

// AskConfirm prompts the user with the provided message and returns true if the user confirms.
// It allows up to maxConfirmAttempts tries before returning false.
func AskConfirm(prompt string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)

	message := strings.TrimSpace(prompt)
	if message == "" {
		message = "Proceed?"
	}

	fmt.Println()
	for i := range maxConfirmAttempts {
		fmt.Printf("%s [y/N]: ", message)
		response, err := reader.ReadString('\n')
		if err != nil {
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
				fmt.Println("Please answer with 'yes'/'no' or 'y'/'n'.")
			}
		}
	}

	return false, nil
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

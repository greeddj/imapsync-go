package main

import (
	"log"

	"github.com/greeddj/imapsync-go/cmd"
)

func main() {
	err := cmd.Run()
	if err != nil {
		log.Fatalf("Error: %v", err)
	}
}

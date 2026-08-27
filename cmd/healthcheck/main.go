package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

const usage = "usage: watchtrace-healthcheck URL"

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func main() {
	client := &http.Client{Timeout: 3 * time.Second}
	if err := check(context.Background(), client, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(ctx context.Context, client httpDoer, arguments []string) error {
	if len(arguments) != 1 || client == nil {
		return errors.New(usage)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, arguments[0], nil)
	if err != nil {
		return errors.New("invalid health-check URL")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("health check failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("health check returned HTTP %d", response.StatusCode)
	}
	return nil
}

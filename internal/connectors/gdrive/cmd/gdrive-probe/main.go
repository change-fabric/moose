// Command gdrive-probe is a dev-only manual check of the Drive connector
// against a real service account key. It is not shipped.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/change-fabric/moose/internal/connectors/gdrive"
	"github.com/change-fabric/moose/internal/core"
)

func main() {
	key := flag.String("key", os.Getenv("MOOSE_GDRIVE_KEY_FILE"), "path to service account JSON key")
	q := flag.String("q", "", "optional search text")
	get := flag.String("get", "", "optional document ID to fetch and print")
	flag.Parse()
	if err := run(*key, *q, *get); err != nil {
		fmt.Fprintln(os.Stderr, "gdrive-probe:", err)
		os.Exit(1)
	}
}

func run(keyPath, q, getID string) error {
	if keyPath == "" {
		return fmt.Errorf("--key is required")
	}
	keyJSON, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := gdrive.New(ctx, keyJSON)
	if err != nil {
		return err
	}
	if err := c.Health(ctx); err != nil {
		return fmt.Errorf("health: %w", err)
	}
	fmt.Println("service account:", c.Detail(ctx))
	res, err := c.List(ctx, "", 10)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	fmt.Println("shared items:")
	for _, it := range res.Items {
		fmt.Printf("  %s  %s  %s\n", it.ID, it.MimeType, it.Title)
	}
	if q != "" {
		sr, err := c.Search(ctx, core.SearchQuery{Text: q, Limit: 10})
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}
		fmt.Printf("search %q:\n", q)
		for _, it := range sr.Items {
			fmt.Printf("  %s  %s  %s\n", it.ID, it.MimeType, it.Title)
		}
	}
	if getID != "" {
		d, err := c.Get(ctx, getID, 0)
		if err != nil {
			return fmt.Errorf("get: %w", err)
		}
		fmt.Printf("get %s: %s (%s, truncated=%v) %s\n%s\n", d.ID, d.Title, d.ContentType, d.Truncated, d.Snippet, d.Content)
	}
	return nil
}

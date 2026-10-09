//go:build integration

package store

import "os"

func getenv(k string) string { return os.Getenv(k) }

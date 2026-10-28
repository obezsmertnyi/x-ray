package main

import (
	"bytes"
	"testing"
)

// An unset provider must not change the derived key, or adding one logs everybody out.
func TestSessionHMACKey_UnchangedByEmptyProvider(t *testing.T) {
	if !bytes.Equal(sessionHMACKey("gh", "az"), sessionHMACKey("gh", "az", "")) {
		t.Fatal("an empty secret changed the session key")
	}
	if sessionHMACKey("", "", "") != nil {
		t.Fatal("no secrets must give no key")
	}
}

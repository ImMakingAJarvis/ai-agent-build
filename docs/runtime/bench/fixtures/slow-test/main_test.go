package calc

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	time.Sleep(60 * time.Second) // slow suite on purpose
	os.Exit(m.Run())
}

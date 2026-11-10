package logx

import (
	"log"
	"strings"
	"sync"
	"testing"
)

// captureLog redirects the standard logger for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func TestVerbosefHonorsTheSwitch(t *testing.T) {
	t.Cleanup(func() { SetVerbose(false) })

	SetVerbose(false)
	if Verbose() {
		t.Fatal("Verbose() is true after SetVerbose(false)")
	}
	if out := captureLog(t, func() { Verbosef("quiet %d", 1) }); out != "" {
		t.Fatalf("logged %q while off, want nothing", out)
	}

	SetVerbose(true)
	if !Verbose() {
		t.Fatal("Verbose() is false after SetVerbose(true)")
	}
	out := captureLog(t, func() { Verbosef("loud %d", 2) })
	if !strings.Contains(out, "loud 2") {
		t.Fatalf("logged %q while on, want it to contain %q", out, "loud 2")
	}
}

// Sources call Verbosef from one goroutine per source while the
// indexer sets the switch, so neither may race.
func TestVerbosefIsSafeAcrossGoroutines(t *testing.T) {
	t.Cleanup(func() { SetVerbose(false) })

	captureLog(t, func() {
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for j := range 50 {
					Verbosef("source %d item %d", i, j)
					_ = Verbose()
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				SetVerbose(i%2 == 0)
			}
		}()
		wg.Wait()
	})
}

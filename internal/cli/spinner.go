package cli

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

// spinner animates on a terminal and degrades to one line per update elsewhere (CI logs, pipes).
type spinner struct {
	w      io.Writer
	tty    bool
	mu     sync.Mutex
	msg    string
	done   chan struct{}
	wg     sync.WaitGroup
	frames []string
}

func newSpinner(w io.Writer) *spinner {
	tty := false
	if f, ok := w.(*os.File); ok && os.Getenv("NO_COLOR") == "" {
		tty = term.IsTerminal(int(f.Fd()))
	}
	return &spinner{w: w, tty: tty, done: make(chan struct{}), frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}}
}

func (s *spinner) Start(msg string) {
	s.msg = msg
	if !s.tty {
		fmt.Fprintln(s.w, msg)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(90 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-s.done:
				return
			case <-t.C:
				s.mu.Lock()
				fmt.Fprintf(s.w, "\r\033[2K  \033[36m%s\033[0m %s", s.frames[i%len(s.frames)], s.msg)
				s.mu.Unlock()
			}
		}
	}()
}

func (s *spinner) Update(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()
	if !s.tty {
		fmt.Fprintln(s.w, "  "+msg)
	}
}

// Stop ends the animation and prints a final status line.
func (s *spinner) Stop(ok bool, msg string) {
	if s.tty {
		close(s.done)
		s.wg.Wait()
		mark := "\033[32m✓\033[0m"
		if !ok {
			mark = "\033[31m✗\033[0m"
		}
		fmt.Fprintf(s.w, "\r\033[2K  %s %s\n", mark, msg)
		return
	}
	fmt.Fprintln(s.w, msg)
}

package services

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/textmem"
)

func (s *Services) initTextMemory(d Deps) {
	dir := filepath.Join(d.DataDir, "memory", "text")
	tm := textmem.New(dir)
	if err := tm.Start(); err != nil {
		log.Printf("[main] warning: text memory start: %v", err)
		s.TextMemory = tm
		return
	}
	log.Printf("[main] text memory active at %s", dir)
	s.TextMemory = tm
	s.Closers = append(s.Closers, tm.Stop)
}

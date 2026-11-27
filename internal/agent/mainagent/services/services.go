package services

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory/pipeline"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory/social"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/textmem"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/tracker"
)

// Services 是主 agent 私有服务的集合。
type Services struct {
	Memory     *memory.GraphDB
	Indexer    *memory.Indexer
	Social     *social.SocialStore
	DocStore   *coldcontext.Store
	TextMemory *textmem.Memory
	Tracker    *tracker.Tracker
	Embedder   *memory.StaticEmbedder
	Distiller  *pipeline.Distiller
	Closers    []func()
	quiescers  []struct {
		Name          string
		Pause, Resume func() error
	}
}

func (s *Services) quiesce(name string, pause, resume func() error) {
	s.quiescers = append(s.quiescers, struct {
		Name          string
		Pause, Resume func() error
	}{name, pause, resume})
}

// Quiescers 返回伪关机钩子（迁移数据目录用）。
func (s *Services) Quiescers() []struct {
	Name          string
	Pause, Resume func() error
} {
	return s.quiescers
}

// Deps 是装配私有服务所需的**全部外部输入**（配置读面 + 数据目录）。
//
// ★ 刻意是值类型而不是内核挂载点：主 agent 的私有服务不该认识 kstate
//
//	（docs §0.1：类型是用户空间，取不到内核表）。
type Deps struct {
	Cfg     kernelapi.ConfigStore
	DataDir string
}

// Install 按序装配全部私有服务（失败项留 nil，与迁移前行为一致）。
func Install(d Deps) *Services {
	s := &Services{}
	s.initMemory(d)
	s.initDocStore(d)
	s.initTextMemory(d)
	return s
}

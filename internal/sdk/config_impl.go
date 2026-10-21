package sdk

import "github.com/JianFeeeee/HologramAgent/pkg/types"

// configImpl 持有运行期配置对象的引用（与配置数据库无关）。
type configImpl struct {
	cfg *types.Config
}

func NewConfig(cfg *types.Config) ConfigAPI {
	return &configImpl{cfg: cfg}
}

func (c *configImpl) Get() *types.Config {
	return c.cfg
}

func (c *configImpl) Put(cfg *types.Config) {
	c.cfg = cfg
}

var _ ConfigAPI = (*configImpl)(nil)

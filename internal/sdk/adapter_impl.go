package sdk

import (
	"errors"
	"os"
	"path/filepath"

	luaVM "github.com/JianFeeeee/HologramAgent/internal/lua"
)

// adapterImpl 桥接 Lua VM 的协议适配器管理。
type adapterImpl struct {
	vm *luaVM.VM
}

func NewAdapter(vm *luaVM.VM) AdapterAPI {
	return &adapterImpl{vm: vm}
}

func (a *adapterImpl) List() []APIAdapter {
	if a.vm == nil {
		return nil
	}
	got := a.vm.ListAdapters()
	out := make([]APIAdapter, len(got))
	for i, ad := range got {
		out[i] = APIAdapter{Name: ad.Name, Version: ad.Version}
	}
	return out
}

func (a *adapterImpl) Load(name, code string) error {
	if a.vm == nil {
		return errors.New("lua vm not available")
	}
	path := filepath.Join(a.vm.AdapterDir(), name+".lua")
	if err := os.WriteFile(path, []byte(code), 0644); err != nil {
		return err
	}
	return a.vm.LoadAdapter(path)
}

func (a *adapterImpl) Remove(name string) error {
	if a.vm == nil {
		return errors.New("lua vm not available")
	}
	path := filepath.Join(a.vm.AdapterDir(), name+".lua")
	if err := os.Remove(path); err != nil {
		return err
	}
	a.vm.RemoveAdapter(name)
	return nil
}

func (a *adapterImpl) AdapterDir() string {
	if a.vm == nil {
		return ""
	}
	return a.vm.AdapterDir()
}

var _ AdapterAPI = (*adapterImpl)(nil)

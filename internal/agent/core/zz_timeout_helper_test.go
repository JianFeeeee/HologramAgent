package core

import sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"

func sdkToolDefWithTimeout(name, timeout string) sdk.ToolDef {
	return sdk.ToolDef{Name: name, Description: "test", Timeout: timeout}
}

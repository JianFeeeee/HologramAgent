// 共享值类型的**唯一声明点**在 internal/kernelapi/model（见该包注释）。
//
// 本文件把它重新导出为本包的名字，使既有调用点（`social.PersonProfile` 等）
// **逐字不变**：类型恒等（别名而非新类型）。
package social

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"

// PersonProfile 是人物特质与关系网的一等值。
type PersonProfile = model.PersonProfile

// SocialRelation 是人物关系的一条边。
type SocialRelation = model.SocialRelation

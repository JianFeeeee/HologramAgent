package social

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

const (
	entityTypePerson = "person"
	entityTypeTrait  = "trait_value"
	traitPrefix      = "trait:"
)

type PersonProfile struct {
	Name      string            `json:"name"`
	Traits    map[string]string `json:"traits,omitempty"`
	Relations []SocialRelation  `json:"relations,omitempty"`
}

type SocialRelation struct {
	Person   string `json:"person"`
	Relation string `json:"relation"` // 关系类型：朋友/家人/同事/邻居/...
}

type SocialStore struct {
	db *memory.GraphDB
	mu sync.RWMutex
}

func New(db *memory.GraphDB) *SocialStore {
	return &SocialStore{db: db}
}

// GetPerson 获取人物完整档案（特质 + 社交关系）
func (s *SocialStore) GetPerson(name string) (*PersonProfile, error) {
	if s.db == nil {
		return nil, fmt.Errorf("social store not available")
	}

	result, err := s.db.Recall([]string{name}, nil, 2, "")
	if err != nil {
		return nil, err
	}

	profile := &PersonProfile{
		Name:   name,
		Traits: make(map[string]string),
	}

	// 查找指定 person 的 ID
	// ★ 按**名字**匹配，不按 ID（2026-10-04）
	//
	// 旧实现：先找出 personID（e.ID），再用 `r.SourceID == personID` 匹配关系。
	// ★★ 块侧这不再成立：e.ID 是「本次召回内的序号」（int64），
	//   而 Relation.SourceID 是**块 ID**（blk_ent_<hash>）——
	//   两个不同 ID 空间，永远匹配不上。
	//
	//   而「张三的特质」这个语义本来就与 ID 无关，只关乎名字。
	//   ★ 用 ID 匹配还有个隐患：同一个人可能有两个块
	//   （历史数据里 blk_ent_a 与 blk_ent_b 文本相同），
	//   按 ID 只认其中一个，另一个的特质就丢了。
	//
	// 先确认人在召回结果里 —— 找不到就报错，不静默返回空档案。
	found := false
	for _, e := range result.Entities {
		if e.Name == name {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("person '%s' not found", name)
	}

	// 区分 trait 关系和社交关系
	for _, r := range result.Relations {
		isSource := r.SourceName == name
		isTarget := r.TargetName == name
		if !isSource && !isTarget {
			continue
		}
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			// 特质：trait:<特质名>，值在另一端
			traitName := strings.TrimPrefix(r.RelationType, traitPrefix)
			other := r.TargetName
			if !isSource {
				other = r.SourceName
			}
			profile.Traits[traitName] = other
			continue
		}
		other := r.TargetName
		if !isSource {
			other = r.SourceName
		}
		profile.Relations = append(profile.Relations, SocialRelation{
			Person:   other,
			Relation: r.RelationType,
		})
	}

	return profile, nil
}

// SetTrait 设置/更新人物特质。如果同名特质已存在则覆盖
func (s *SocialStore) SetTrait(name, trait, value string) error {
	if s.db == nil {
		return fmt.Errorf("social store not available")
	}

	// 先清除旧特质值
	oldVal, found := s.GetTrait(name, trait)
	if found && oldVal != "" {
		s.db.Purge(map[string]string{
			"subject_contains": name,
			"relation_type":    traitPrefix + trait,
		}, "soft")
	}

	triples := []memory.Triple{
		{
			Subject:     name,
			SubjectType: entityTypePerson,
			Relation:    traitPrefix + trait,
			Object:      value,
			ObjectType:  entityTypeTrait,
			Confidence:  1.0,
		},
	}
	_, _, err := s.db.Commit(triples, "social_trait", 0)
	return err
}

// GetTrait 获取指定人物的指定特质值
func (s *SocialStore) GetTrait(name, trait string) (string, bool) {
	if s.db == nil {
		return "", false
	}

	result, err := s.db.Recall([]string{name}, nil, 1, "")
	if err != nil || result == nil {
		return "", false
	}

	// ★ 按名字匹配，不按 ID（2026-10-04）
	//
	// 旧实现找 personID 再用 `r.SourceID == personID` 过滤。
	// ★★ 块侧不成立：e.ID 是召回内的序号（int64），
	//   Relation.SourceID 是块 ID（字符串）—— 两个 ID 空间，永不匹配。
	//
	//   症状极具迷惑性：第 17 行 `return r.SourceName` 在
	//   「SourceID != personID」时**无条件**执行，于是 GetTrait 返回
	//   **人物自己的名字**当特质值（判据里是 got "张三" 而非空串）——
	//   看不出是 ID 空间错配，只像是数据错了。
	wantRel := traitPrefix + trait
	for _, r := range result.Relations {
		if r.RelationType != wantRel {
			continue
		}
		if r.SourceName == name {
			return r.TargetName, true
		}
		if r.TargetName == name {
			return r.SourceName, true
		}
	}
	return "", false
}

// AddRelation 建立两人之间的社交关系
func (s *SocialStore) AddRelation(personA, relation, personB string) error {
	if s.db == nil {
		return fmt.Errorf("social store not available")
	}

	triples := []memory.Triple{
		{
			Subject:     personA,
			SubjectType: entityTypePerson,
			Relation:    relation,
			Object:      personB,
			ObjectType:  entityTypePerson,
			Confidence:  1.0,
		},
	}
	_, _, err := s.db.Commit(triples, "social_relation", 0)
	return err
}

// RemoveRelation 删除两人之间的社交关系
func (s *SocialStore) RemoveRelation(personA, relation, personB string) error {
	if s.db == nil {
		return fmt.Errorf("social store not available")
	}

	_, err := s.db.Purge(map[string]string{
		"subject_contains": personA,
		"target_contains":  personB,
		"relation_type":    relation,
	}, "soft")
	return err
}

// GetRelations 获取指定人物的所有社交关系
func (s *SocialStore) GetRelations(name string) ([]SocialRelation, error) {
	if s.db == nil {
		return nil, fmt.Errorf("social store not available")
	}

	result, err := s.db.Recall([]string{name}, nil, 1, "")
	if err != nil {
		return nil, err
	}

	// ★ 按名字匹配，不按 ID —— 见 GetTrait 的说明：
	//   Entity.ID 是召回内序号，Relation.SourceID/TargetID 是块 ID，
	//   两个 ID 空间，按 ID 匹配永远为假。
	//
	// ★ 顺带修掉的隐患：旧实现找不到 personID 就 return nil（空列表），
	//   而「人不在库里」与「人存在但没有关系」是两件事 ——
	//   前者是错误，后者是事实。空列表让调用方无从区分。
	var relations []SocialRelation
	for _, r := range result.Relations {
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			continue
		}
		if r.SourceName == name {
			relations = append(relations, SocialRelation{Person: r.TargetName, Relation: r.RelationType})
		} else if r.TargetName == name {
			relations = append(relations, SocialRelation{Person: r.SourceName, Relation: r.RelationType})
		}
	}
	return relations, nil
}

// GetNetwork 获取指定人物周围 depth 度的社交网络
func (s *SocialStore) GetNetwork(name string, depth int) ([]*PersonProfile, error) {
	if s.db == nil {
		return nil, fmt.Errorf("social store not available")
	}

	// 用 Recall 的 BFS 遍历获取多度关联
	result, err := s.db.Recall([]string{name}, nil, depth, "")
	if err != nil {
		return nil, err
	}

	personMap := make(map[int64]*PersonProfile)
	for _, e := range result.Entities {
		p := &PersonProfile{
			Name:   e.Name,
			Traits: make(map[string]string),
		}
		personMap[e.ID] = p
	}

	for _, r := range result.Relations {
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			traitName := strings.TrimPrefix(r.RelationType, traitPrefix)
			if p, ok := personMap[r.SourceID]; ok {
				p.Traits[traitName] = r.TargetName
			}
			if p, ok := personMap[r.TargetID]; ok {
				p.Traits[traitName] = r.SourceName
			}
		}
	}

	// 收集关系
	for _, r := range result.Relations {
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			continue
		}
		sr := SocialRelation{Relation: r.RelationType}
		if p, ok := personMap[r.SourceID]; ok {
			sr.Person = r.TargetName
			p.Relations = append(p.Relations, sr)
		}
		sr = SocialRelation{Relation: r.RelationType}
		if p, ok := personMap[r.TargetID]; ok {
			sr.Person = r.SourceName
			p.Relations = append(p.Relations, sr)
		}
	}

	var profiles []*PersonProfile
	for _, p := range personMap {
		profiles = append(profiles, p)
	}
	return profiles, nil
}

// ListPersons 列出所有已知人物（entity.type = person）
func (s *SocialStore) ListPersons() ([]string, error) {
	if s.db == nil {
		return nil, fmt.Errorf("social store not available")
	}

	// ★★★ 改从**图结构**推断人物（2026-10-04）
	//
	// 旧实现靠 `e.Type == "person"` 筛选。块侧没有 type：
	//   Triple.SubjectType / ObjectType 只写旧 entities.type，
	//   memory_blocks 里**没有任何类型列**。
	//
	// ★ 而「谁是人物」在社交图里本来就是**结构可判定的**：
	//
	//	人物 = 社交关系（非 trait 关系）的任一端点
	//	     ∪ trait 关系的**源**（特质挂在主体上，trait:性格→开朗）
	//
	//   这不是权宜之计 —— 它比 type 更可靠：type 是写的时候声明的
	//   （调用方可能不声明、可能声明错），而结构是数据本身的性质。
	//   旧实现里「AddRelation 加了人但没 SubjectType」就会漏掉那个人。
	//
	// ★ 保留一个回退：若图里一条边都没有（纯空库），
	//   返回空列表而不是全部 —— 那才是「没有人物」。
	// ★ 改走 RecentRelations（2026-10-08 修 130ms）
	//
	// 原实现用 Recall(nil, nil, 1, "")，而那条 full-recall 路径为了填
	// result.Entities 会加载**全部块（含稠密向量）**。生产实测：
	//
	//	Recall 全程 130ms：实体 3266 个、关系 30 条
	//
	// 即为了 30 条关系白付了 3266 个块的向量反序列化。而本函数只看
	// result.Relations。改成只取关系（同一 WHERE/ORDER/LIMIT，4ms）。
	rels, err := s.db.RecentRelations(30)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	for _, r := range rels {
		if strings.HasPrefix(r.RelationType, traitPrefix) {
			// 特质：源是主体（人物），目标是特质值
			seen[r.SourceName] = true
			continue
		}
		// 社交关系：两端都是人物
		seen[r.SourceName] = true
		seen[r.TargetName] = true
	}

	persons := make([]string, 0, len(seen))
	for name := range seen {
		if name != "" {
			persons = append(persons, name)
		}
	}
	// ★ 排序：Recall 的行序由 SQLite 决定，不稳定。
	//   列表接口的输出必须可复现，否则测试与展示都会飘。
	sort.Strings(persons)
	return persons, nil
}

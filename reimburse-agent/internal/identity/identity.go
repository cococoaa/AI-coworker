// Package identity 是"工号 + 令牌"的最小人员身份。
//
// 为什么需要它：令牌是机器凭证（一个令牌可能被多人共用、写进配置、复制给同事），
// 它只能回答"这个部署能不能访问"，回答不了"这件事是谁做的"。而复核结论会写进
// 查重库、影响后续单据，必须能追到人。
//
// 设计取舍：
//   - **不做 RBAC。** 当前暴露的写操作只有"触发审核"和"写复核结论"两个，
//     真正的职责分离（谁能改制度）已经在服务器访问权那一层，硬做 scope 只会
//     做出一个看起来像权限、实际追不到人的假角色。
//   - **令牌只证明"这个部署允许访问"，工号才是身份。** 所以两者都要，
//     令牌错、工号不在名单，都拒绝。
//   - **工号名单来自配置文件，不由前端自称。** 前端传什么就是什么，等于没有身份。
//
// 身份只用于两件事：审计留痕、配额分桶。它不改变判定逻辑。
package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Person 是一个可以使用系统的人。
type Person struct {
	// EmployeeID 是工号，唯一标识。
	EmployeeID string `json:"employee_id"`
	Name       string `json:"name"`
	// Dept 是部门，用于展示与分桶。
	Dept string `json:"dept,omitempty"`
	// CanReview 表示能否提交复核结论（把人从"只看"改成"能签批"）。
	CanReview bool `json:"can_review"`
	// CanAudit 表示能否触发审核。
	CanAudit bool `json:"can_audit"`
	// Disabled 为真时拒绝该工号（离职、停用），但保留历史记录。
	Disabled bool `json:"disabled,omitempty"`
}

// Registry 是工号名单。
type Registry struct {
	mu     sync.RWMutex
	people map[string]*Person
	// now 便于测试。
	now func() time.Time
}

// ErrUnknown 表示工号不在名单里。
var ErrUnknown = errors.New("工号不在授权名单中")

// New 用一份名单创建注册表。
func New(people []Person) (*Registry, error) {
	r := &Registry{people: map[string]*Person{}, now: time.Now}
	for i := range people {
		p := people[i]
		p.EmployeeID = strings.TrimSpace(p.EmployeeID)
		if p.EmployeeID == "" {
			return nil, fmt.Errorf("第 %d 个人员缺少工号", i+1)
		}
		if _, dup := r.people[p.EmployeeID]; dup {
			return nil, fmt.Errorf("工号重复: %s", p.EmployeeID)
		}
		r.people[p.EmployeeID] = &p
	}
	return r, nil
}

// LoadFile 从 JSON 文件读取名单。
// 文件不存在时返回空名单（此时任何复核都会被拒绝——默认拒绝，不是默认放行）。
func LoadFile(path string) (*Registry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(nil)
		}
		return nil, err
	}
	var f struct {
		People []Person `json:"people"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("解析人员名单 %s: %w", path, err)
	}
	return New(f.People)
}

// Lookup 按工号取人。停用的也返回，由调用方判断。
func (r *Registry) Lookup(id string) (*Person, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.people[strings.TrimSpace(id)]
	if !ok {
		return nil, false
	}
	cp := *p
	return &cp, true
}

// Authenticate 校验"工号能不能代表这个人做事"。
// action 取 "audit" 或 "review"。
func (r *Registry) Authenticate(id, action string) (*Person, error) {
	p, ok := r.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if p.Disabled {
		return nil, fmt.Errorf("工号 %s 已停用", id)
	}
	switch action {
	case "review":
		if !p.CanReview {
			return nil, fmt.Errorf("工号 %s（%s）没有复核权限", p.EmployeeID, p.Name)
		}
	case "audit":
		if !p.CanAudit {
			return nil, fmt.Errorf("工号 %s（%s）没有触发审核的权限", p.EmployeeID, p.Name)
		}
	}
	return p, nil
}

// All 返回全部人员，按工号排序。
func (r *Registry) All() []Person {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Person, 0, len(r.people))
	for _, p := range r.people {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EmployeeID < out[j].EmployeeID })
	return out
}

// Size 返回名单人数。
func (r *Registry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.people)
}

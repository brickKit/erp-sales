// leafDeptID 是纯函数，不需要真实数据库/gRPC 依赖——阶段三 Task 6
// 新增，见 create.go 同名函数注释。
package tcc

import "testing"

func TestLeafDeptID(t *testing.T) {
	cases := []struct {
		deptPath string
		want     string
	}{
		{"/1/12/", "12"},
		{"/1/", "1"},
		{"", ""},   // 根节点：没有具体部门，dept_id 也应该是空
		{"/", ""},  // 边界：只有分隔符，等价于根节点
		{"1/12/", "12"}, // 边界：万一没有前导斜杠也不该崩
	}
	for _, c := range cases {
		if got := leafDeptID(c.deptPath); got != c.want {
			t.Errorf("leafDeptID(%q) = %q，期望 %q", c.deptPath, got, c.want)
		}
	}
}

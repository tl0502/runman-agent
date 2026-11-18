package config

import (
	"reflect"
	"testing"
)

// 端口白名单的解析必须兼容用户从聊天/文档里粘贴的中文标点：
// 旧实现只按 ASCII 逗号切分，"779，22818" 会让整串 Atoi 失败，
// 静默解析成空列表，白名单全丢且无任何提示。
func TestParsePortList(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"779,22818", []int{779, 22818}},
		{"779，22818", []int{779, 22818}},          // 中文逗号
		{"779、22818", []int{779, 22818}},          // 顿号
		{"779；22818", []int{779, 22818}},          // 中文分号
		{"779 22818", []int{779, 22818}},          // 空格
		{"22818\n779\t80", []int{80, 779, 22818}}, // 换行/制表符 + 升序
		{"  ", nil}, // 全空
		{"", nil},
		{"0,65536,abc,-1", nil}, // 越界/非法全部丢弃
		{"80,80,80", []int{80}}, // 去重
		{"1, 2 ,3", []int{1, 2, 3}},
	}
	for _, c := range cases {
		got := ParsePortList(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParsePortList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// WriteJSON 以缩进格式写 JSON，不转义 < > &，保证中文可读。
func WriteJSON(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// WriteCase 把一条用例写成目录：claim.json、invoice.txt、support.txt、truth.json、world.json。
func WriteCase(root string, c *Case) error {
	dir := filepath.Join(root, "cases", c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := WriteJSON(filepath.Join(dir, "claim.json"), c.Claim); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "invoice.txt"), []byte(c.InvoiceText), 0o644); err != nil {
		return err
	}
	if c.SupportText != "" {
		if err := os.WriteFile(filepath.Join(dir, "support.txt"), []byte(c.SupportText), 0o644); err != nil {
			return err
		}
	}
	if err := WriteJSON(filepath.Join(dir, "truth.json"), c.Truth); err != nil {
		return err
	}
	return WriteJSON(filepath.Join(dir, "world.json"), c.World)
}

// LoadCase 读取一条用例目录。
func LoadCase(dir string) (*Case, error) {
	c := &Case{ID: filepath.Base(dir)}
	if err := readJSON(filepath.Join(dir, "claim.json"), &c.Claim); err != nil {
		return nil, fmt.Errorf("%s claim.json: %w", c.ID, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, c.Claim.Attachments.InvoiceFile))
	if err != nil {
		return nil, fmt.Errorf("%s 发票文本: %w", c.ID, err)
	}
	c.InvoiceText = string(b)
	if f := c.Claim.Attachments.SupportFile; f != "" {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, fmt.Errorf("%s 佐证凭证文本: %w", c.ID, err)
		}
		c.SupportText = string(b)
	}
	if err := readJSON(filepath.Join(dir, "truth.json"), &c.Truth); err != nil {
		return nil, fmt.Errorf("%s truth.json: %w", c.ID, err)
	}
	if err := readJSON(filepath.Join(dir, "world.json"), &c.World); err != nil {
		return nil, fmt.Errorf("%s world.json: %w", c.ID, err)
	}
	return c, nil
}

// LoadAll 读取测试集目录下全部用例，按编号排序。
func LoadAll(root string) ([]*Case, error) {
	entries, err := os.ReadDir(filepath.Join(root, "cases"))
	if err != nil {
		return nil, err
	}
	var cases []*Case
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := LoadCase(filepath.Join(root, "cases", e.Name()))
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

// MergeWorlds 合并多条用例的后端数据，供演示界面使用。按主键去重，保持先后顺序。
func MergeWorlds(cases []*Case) World {
	var w World
	seenInv, seenCo, seenHist := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range cases {
		for _, r := range c.World.Invoices {
			if !seenInv[r.InvoiceNo] {
				seenInv[r.InvoiceNo] = true
				w.Invoices = append(w.Invoices, r)
			}
		}
		for _, co := range c.World.Companies {
			if !seenCo[co.TaxID] {
				seenCo[co.TaxID] = true
				w.Companies = append(w.Companies, co)
			}
		}
		for _, h := range c.World.History {
			if !seenHist[h.ClaimNo] {
				seenHist[h.ClaimNo] = true
				w.History = append(w.History, h)
			}
		}
		w.Faults = append(w.Faults, c.World.Faults...)
	}
	return w
}

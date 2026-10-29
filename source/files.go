package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type pathSet struct {
	files       string // 可供预览的文件目录
	data        string // 数据目录
	db          string // data/app.json
	hostsBackup string // data/hosts.backup（hosts 原始备份）
}

func newPathSet(base string) pathSet {
	data := filepath.Join(base, "data")
	return pathSet{
		files:       filepath.Join(base, "files"),
		data:        data,
		db:          filepath.Join(data, "app.json"),
		hostsBackup: filepath.Join(data, "hosts.backup"),
	}
}

type fileInfo struct {
	Path  string `json:"path"` // 相对 files 目录的路径，统一用 / 分隔
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

// listFiles 递归列出 files 目录下的普通文件（忽略以 . 开头的文件和目录）
func listFiles(root string) ([]fileInfo, error) {
	out := []fileInfo{}
	if _, err := os.Stat(root); err != nil {
		return out, nil
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过无法访问的条目
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") && path != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		out = append(out, fileInfo{
			Path:  filepath.ToSlash(rel),
			Name:  name,
			Size:  info.Size(),
			MTime: info.ModTime().Unix(),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path)
	})
	return out, err
}

// resolveFile 把前端传来的相对路径解析为绝对路径，并确保它位于 files 目录内（防止路径穿越）
func resolveFile(root, rel string) (string, bool) {
	if rel == "" || strings.ContainsAny(rel, "\\\x00") || strings.HasPrefix(rel, "/") {
		return "", false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return "", false
		}
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	if !strings.HasPrefix(resolved, base+string(filepath.Separator)) {
		return "", false
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return resolved, true
}

var mimeOverrides = map[string]string{
	".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
	".md": "text/markdown; charset=utf-8", ".csv": "text/csv; charset=utf-8",
	".json": "application/json; charset=utf-8", ".xml": "application/xml; charset=utf-8",
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8",
	".ps1": "text/plain; charset=utf-8", ".bat": "text/plain; charset=utf-8",
	".py": "text/plain; charset=utf-8", ".go": "text/plain; charset=utf-8",
	".yaml": "text/plain; charset=utf-8", ".yml": "text/plain; charset=utf-8",
	".ini": "text/plain; charset=utf-8",
	".pdf": "application/pdf",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".svg": "image/svg+xml", ".ico": "image/x-icon",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".flac": "audio/flac",
	".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/ogg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".mp4": "video/mp4", ".mkv": "video/x-matroska", ".avi": "video/x-msvideo", ".webm": "video/webm", ".ogv": "video/ogg", ".mov": "video/quicktime",
	".zip": "application/zip", ".7z": "application/x-7z-compressed",
	".rar": "application/vnd.rar", ".gz": "application/gzip", ".tar": "application/x-tar",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// guessType 根据后缀返回 Content-Type；未知后缀为 application/octet-stream
func guessType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := mimeOverrides[ext]; ok {
		return t
	}
	return "application/octet-stream"
}

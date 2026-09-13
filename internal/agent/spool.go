package agent

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	spoolPrefix      = "spool-"
	deadLetterPrefix = "dead-letter-"
	jsonlSuffix      = ".jsonl"
	gzSuffix         = ".gz"
	offsetFileName   = ".offset"
	syncEveryN       = 100
	dateLayout       = "2006-01-02"
)

// Position 指向 spool 中下一筆尚未轉送的位置。
type Position struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
}

func (p Position) less(q Position) bool {
	if p.File != q.File {
		return p.File < q.File
	}
	return p.Offset < q.Offset
}

type spoolFile struct {
	name string
	date string
	size int64
	gz   bool
}

// Spool 管理按日分檔的 JSONL spool：receiver 寫入、forwarder 讀取與推進 offset、janitor 清理。
// 檔名以 UTC 日期命名，且寫入的檔名只會遞增（系統時間倒退時沿用目前檔案），
// 因此「已有更新的檔案」即代表較舊的檔案不會再被寫入。
type Spool struct {
	dir string
	log *slog.Logger

	wmu       sync.Mutex // 保護寫入端
	cur       *os.File
	curName   string
	curSize   int64
	unsynced  int
	writeHook func() error // 測試用：注入寫入錯誤以模擬磁碟寫滿

	pmu       sync.Mutex // 保護讀取位置；讀取批次與刪檔互斥
	committed Position   // 已確認送達（已寫入 offset 檔）
	inflight  Position   // 已讀出、正在轉送的批次結尾
}

func OpenSpool(dir string, log *slog.Logger) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("建立 spool 目錄失敗：%w", err)
	}
	s := &Spool{dir: dir, log: log}
	s.repairTails()
	s.committed = s.loadOffset()
	s.inflight = s.committed
	return s, nil
}

func spoolName(t time.Time) string { return spoolPrefix + t.UTC().Format(dateLayout) + jsonlSuffix }

// ---- 寫入 ----

// Append 寫入一行（含換行）。每筆寫入即交給作業系統；每 100 筆 fsync 一次，另由外部每秒呼叫 Sync。
func (s *Spool) Append(t time.Time, line []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.writeHook != nil {
		if err := s.writeHook(); err != nil {
			return err
		}
	}
	if name := spoolName(t); s.cur == nil || name > s.curName {
		if err := s.openLocked(name); err != nil {
			return err
		}
	}
	n, err := s.cur.Write(line)
	if err != nil {
		// 寫入失敗（例如磁碟已滿）：截掉可能殘留的半行
		_ = s.cur.Truncate(s.curSize)
		return err
	}
	s.curSize += int64(n)
	s.unsynced++
	if s.unsynced >= syncEveryN {
		return s.syncLocked()
	}
	return nil
}

func (s *Spool) openLocked(name string) error {
	if s.cur == nil {
		// 啟動後第一次寫入：若已有日期較新的檔案（系統時間曾倒退），沿用它以維持檔名遞增
		if names := s.plainNames(); len(names) > 0 && names[len(names)-1] > name {
			name = names[len(names)-1]
		}
	} else {
		_ = s.syncLocked()
		_ = s.cur.Close()
		s.cur = nil
		s.log.Info("spool 切換新檔", "file", name, "previous", s.curName)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	s.cur, s.curName, s.curSize, s.unsynced = f, name, st.Size(), 0
	return nil
}

func (s *Spool) Sync() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.syncLocked()
}

func (s *Spool) syncLocked() error {
	if s.cur == nil || s.unsynced == 0 {
		return nil
	}
	s.unsynced = 0
	return s.cur.Sync()
}

func (s *Spool) Close() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.cur == nil {
		return nil
	}
	_ = s.syncLocked()
	err := s.cur.Close()
	s.cur = nil
	return err
}

func (s *Spool) currentName() string {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.curName
}

func (s *Spool) setWriteHook(h func() error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.writeHook = h
}

// ---- 讀取與 offset ----

// ReadBatch 從已確認的位置讀取最多 max 筆完整的行，可跨檔。
// 檔尾未以換行結束的殘缺行不會被讀取；格式損壞的行記錄 WARN 後略過。
func (s *Spool) ReadBatch(max int) ([]json.RawMessage, Position, error) {
	return s.readBatch(max, true)
}

// ReadReadyBatch reserves a short batch only when the caller will deliver it.
// A preview discarded while waiting for the batch interval must not suppress drops.
func (s *Spool) ReadReadyBatch(max int, allowPartial bool) ([]json.RawMessage, Position, error) {
	return s.readBatch(max, allowPartial)
}

func (s *Spool) readBatch(max int, allowPartial bool) ([]json.RawMessage, Position, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	pos := s.committed
	names := s.plainNames()
	var lines []json.RawMessage
	for len(lines) < max {
		idx := sort.SearchStrings(names, pos.File)
		if idx == len(names) {
			break
		}
		if names[idx] != pos.File {
			// offset 指向的檔已不存在（被清理，或尚無 offset）：從下一個檔開頭繼續
			pos = Position{File: names[idx]}
		}
		eof, err := s.readFrom(&pos, max, &lines)
		if err != nil {
			return nil, s.committed, err
		}
		if !eof || idx == len(names)-1 {
			break
		}
		pos = Position{File: names[idx+1]}
	}
	if len(lines) < max && !allowPartial {
		return nil, s.committed, nil
	}
	if len(lines) > 0 {
		s.inflight = pos
	}
	return lines, pos, nil
}

func (s *Spool) readFrom(pos *Position, max int, lines *[]json.RawMessage) (eof bool, err error) {
	f, err := os.Open(filepath.Join(s.dir, pos.File))
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if pos.Offset > st.Size() {
		s.log.Warn("offset 超過檔案大小，保守起見從該檔開頭重送", "file", pos.File, "offset", pos.Offset, "size", st.Size())
		pos.Offset = 0
	}
	if _, err := f.Seek(pos.Offset, io.SeekStart); err != nil {
		return false, err
	}
	r := bufio.NewReaderSize(f, 256*1024)
	for len(*lines) < max {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return true, nil
			}
			return false, err
		}
		start := pos.Offset
		pos.Offset += int64(len(line))
		body := bytes.TrimRight(line, "\r\n")
		if len(body) == 0 {
			continue
		}
		if !json.Valid(body) {
			s.log.Warn("略過 spool 中格式損壞的行", "file", pos.File, "offset", start, "bytes", len(line))
			continue
		}
		*lines = append(*lines, json.RawMessage(body))
	}
	return false, nil
}

func (s *Spool) Committed() Position {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return s.committed
}

// Commit 推進 offset，並以「寫暫存檔 → fsync → rename」原子更新 offset 檔。
// 即使寫檔失敗，記憶體中的位置仍會推進（重啟後最多重送，不會遺失）。
func (s *Spool) Commit(p Position) error {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.committed = p
	if s.inflight.less(p) {
		s.inflight = p
	}
	data, _ := json.Marshal(p)
	return writeFileAtomic(filepath.Join(s.dir, offsetFileName), data)
}

func (s *Spool) loadOffset() Position {
	data, err := os.ReadFile(filepath.Join(s.dir, offsetFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return Position{}
	}
	var p Position
	if err == nil {
		err = json.Unmarshal(data, &p)
	}
	if err != nil || !isSpoolPlainName(p.File) || p.Offset < 0 {
		s.log.Warn("offset 檔無法讀取或已損壞，保守起見從最舊的 spool 檔開頭重送", "error", err)
		return Position{}
	}
	return p
}

// Sizes 回傳尚未轉送的位元組數與 spool 檔總量。
func (s *Spool) Sizes() (backlog, total int64) {
	c := s.Committed()
	for _, f := range s.list() {
		total += f.size
		switch {
		case f.gz || f.name < c.File:
		case f.name == c.File:
			backlog += max(f.size-c.Offset, 0)
		default:
			backlog += f.size
		}
	}
	return backlog, total
}

// ---- 啟動修復 ----

// repairTails 捨棄每個 spool 檔尾未完整寫入的行（斷電時可能發生）。
func (s *Spool) repairTails() {
	for _, name := range s.plainNames() {
		path := filepath.Join(s.dir, name)
		cut, err := completeLength(path)
		if err != nil {
			s.log.Warn("檢查 spool 檔尾失敗", "file", name, "error", err)
			continue
		}
		if cut < 0 {
			continue
		}
		if err := os.Truncate(path, cut); err != nil {
			s.log.Error("截斷 spool 殘缺行失敗", "file", name, "error", err)
			continue
		}
		s.log.Warn("已捨棄 spool 檔尾未完整寫入的資料", "file", name, "truncated_to", cut)
	}
}

// completeLength 回傳最後一個換行之後的位置；檔案已完整（空檔或以換行結尾）時回傳 -1。
func completeLength(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	buf := make([]byte, 64*1024)
	for end := size; end > 0; {
		start := max(end-int64(len(buf)), 0)
		chunk := buf[:end-start]
		if _, err := f.ReadAt(chunk, start); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			if cut := start + int64(i) + 1; cut != size {
				return cut, nil
			}
			return -1, nil
		}
		end = start
	}
	if size == 0 {
		return -1, nil
	}
	return 0, nil
}

// ---- 清理 ----

// Cleanup 依保留天數與總量上限刪除舊檔、壓縮已轉送完成的舊檔，回傳因刪除而丟棄的未轉送事件數。
func (s *Spool) Cleanup(now time.Time, retentionDays int, maxBytes int64) (dropped int64) {
	cutoff := now.UTC().AddDate(0, 0, -retentionDays).Format(dateLayout)
	current := s.currentName()

	s.pmu.Lock()
	var kept []spoolFile
	var total int64
	for _, f := range s.list() {
		if f.date < cutoff && f.name != current {
			if n, ok := s.removeLocked(f, "超過保留天數", false); ok {
				dropped += n
				continue
			}
		}
		kept = append(kept, f)
		total += f.size
	}
	for total > maxBytes && len(kept) > 0 {
		f := kept[0]
		if f.name == current {
			s.log.Warn("spool 總量超過上限，但僅剩目前寫入中的檔案，無法再刪除", "total_bytes", total, "max_bytes", maxBytes)
			break
		}
		n, ok := s.removeLocked(f, "spool 總量超過上限", true)
		if !ok {
			break
		}
		dropped += n
		total -= f.size
		kept = kept[1:]
	}
	var toCompress []string
	for _, f := range kept {
		if !f.gz && f.name < s.committed.File && f.name != current {
			toCompress = append(toCompress, f.name)
		}
	}
	s.pmu.Unlock()

	// 已完整轉送的檔案不會再被讀寫，壓縮不需持鎖
	for _, name := range toCompress {
		s.compress(name)
	}
	s.cleanupDeadLetters(cutoff)
	return dropped
}

func (s *Spool) removeLocked(f spoolFile, reason string, warn bool) (dropped int64, ok bool) {
	path := filepath.Join(s.dir, f.name)
	// 正在轉送中的批次仍會送出，因此以 committed 與 inflight 較後者計算丟棄數
	eff := s.committed
	if eff.less(s.inflight) {
		eff = s.inflight
	}
	if !f.gz && f.name >= eff.File {
		var from int64
		if f.name == eff.File {
			from = eff.Offset
		}
		n, err := countLines(path, from)
		if err != nil {
			s.log.Warn("計算未轉送事件數失敗", "file", f.name, "error", err)
		}
		dropped = n
	}
	if err := os.Remove(path); err != nil {
		s.log.Warn("刪除 spool 檔失敗", "file", f.name, "error", err)
		return 0, false
	}
	attrs := []any{"file", f.name, "reason", reason, "bytes", f.size, "events_dropped", dropped}
	if warn || dropped > 0 {
		s.log.Warn("刪除 spool 檔", attrs...)
	} else {
		s.log.Info("刪除 spool 檔", attrs...)
	}
	return dropped, true
}

func (s *Spool) compress(name string) {
	src := filepath.Join(s.dir, name)
	dst := src + gzSuffix
	tmp := dst + ".tmp"
	err := func() error {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			return err
		}
		zw := gzip.NewWriter(out)
		_, err = io.Copy(zw, in)
		if err == nil {
			err = zw.Close()
		}
		if err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	}()
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		s.log.Warn("壓縮 spool 檔失敗", "file", name, "error", err)
		return
	}
	if err := os.Remove(src); err != nil {
		s.log.Warn("壓縮完成但刪除原檔失敗", "file", name, "error", err)
		return
	}
	s.log.Info("spool 檔已壓縮", "file", name+gzSuffix)
}

func (s *Spool) cleanupDeadLetters(cutoff string) {
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, deadLetterPrefix) || !strings.HasSuffix(name, jsonlSuffix) {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, deadLetterPrefix), jsonlSuffix)
		if date >= cutoff {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
			s.log.Warn("刪除 dead-letter 檔失敗", "file", name, "error", err)
			continue
		}
		s.log.Info("刪除 dead-letter 檔", "file", name, "reason", "超過保留天數")
	}
}

// WriteDeadLetter 將一筆 dead-letter 記錄 append 並 fsync。
func (s *Spool) WriteDeadLetter(now time.Time, entry []byte) error {
	path := filepath.Join(s.dir, deadLetterPrefix+now.UTC().Format(dateLayout)+jsonlSuffix)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(entry)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---- 檔案列舉 ----

func (s *Spool) list() []spoolFile { return listSpoolFiles(s.dir) }

func (s *Spool) plainNames() []string {
	var names []string
	for _, f := range s.list() {
		if !f.gz {
			names = append(names, f.name)
		}
	}
	return names
}

func listSpoolFiles(dir string) []spoolFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []spoolFile
	for _, e := range entries {
		name := e.Name()
		gz := strings.HasSuffix(name, jsonlSuffix+gzSuffix)
		if e.IsDir() || !(gz || isSpoolPlainName(name)) {
			continue
		}
		date := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, spoolPrefix), gzSuffix), jsonlSuffix)
		if _, err := time.Parse(dateLayout, date); err != nil || !strings.HasPrefix(name, spoolPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, spoolFile{name: name, date: date, size: info.Size(), gz: gz})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].date != files[j].date {
			return files[i].date < files[j].date
		}
		return !files[i].gz && files[j].gz
	})
	return files
}

func isSpoolPlainName(name string) bool {
	if !strings.HasPrefix(name, spoolPrefix) || !strings.HasSuffix(name, jsonlSuffix) {
		return false
	}
	_, err := time.Parse(dateLayout, strings.TrimSuffix(strings.TrimPrefix(name, spoolPrefix), jsonlSuffix))
	return err == nil
}

func countLines(path string, from int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	var n int64
	buf := make([]byte, 64*1024)
	for {
		k, err := f.Read(buf)
		n += int64(bytes.Count(buf[:k], []byte{'\n'}))
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// writeFileAtomic 寫入暫存檔 → fsync → rename 覆蓋目標檔。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Open(filepath.Dir(path)); err == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return nil
}

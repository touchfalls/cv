package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// streams возвращает "codec_name,codec_tag_string,language" по каждой дорожке.
func streams(t *testing.T, f string) string {
	return strings.TrimSpace(sh(t, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,codec_tag_string:stream_tags=language", "-of", "csv=p=0", f))
}

func TestVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("нет ffmpeg")
	}
	d := t.TempDir()
	srt := filepath.Join(d, "s.srt")
	os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nпривет\n"), 0o644)
	mkv := filepath.Join(d, "in.mkv")
	sh(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2",
		"-f", "lavfi", "-i", "sine=d=2", "-f", "lavfi", "-i", "sine=f=880:d=2", "-i", srt,
		"-map", "0", "-map", "1", "-map", "2", "-map", "3",
		"-c:v", "libx264", "-c:a:0", "aac", "-c:a:1", "flac", "-c:s", "srt",
		"-metadata:s:a:1", "language=rus", mkv)
	ctx := context.Background()

	if run(ctx, opts{}, []string{mkv}) != 0 {
		t.Fatal("mkv→mp4")
	}
	got := streams(t, filepath.Join(d, "in.mp4"))
	for _, want := range []string{"h264,avc1", "aac,mp4a", "aac,mp4a,rus", "mov_text,tx3g"} {
		if !strings.Contains(got, want) {
			t.Errorf("mp4: нет %q в\n%s", want, got)
		}
	}

	back := filepath.Join(d, "back")
	if run(ctx, opts{out: back}, []string{filepath.Join(d, "in.mp4")}) != 0 {
		t.Fatal("mp4→mkv")
	}
	if got := streams(t, filepath.Join(back, "in.mkv")); !strings.Contains(got, "subrip") || strings.Count(got, "aac") != 2 {
		t.Errorf("mkv: %s", got)
	}

	c := filepath.Join(d, "c")
	if run(ctx, opts{compress: true, out: c}, []string{mkv}) != 0 {
		t.Fatal("compress")
	}
	if got := streams(t, filepath.Join(c, "in.mp4")); !strings.HasPrefix(got, "hevc,hvc1") {
		t.Errorf("compress: %s", got)
	}

	// обрезка: первая секунда, имя .cut, длительность ~1с
	cut := filepath.Join(d, "cut")
	if run(ctx, opts{to: 1}, []string{mkv}) != 0 || run(ctx, opts{from: 0.5, to: 1.5, out: cut}, []string{mkv}) != 0 {
		t.Fatal("cut")
	}
	for _, f := range []string{filepath.Join(d, "in.cut.mp4"), filepath.Join(cut, "in.cut.mp4")} {
		dur := strings.TrimSpace(sh(t, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", f))
		if v, _ := strconv.ParseFloat(dur, 64); v < 0.9 || v > 1.6 {
			t.Errorf("%s: длительность %s", f, dur)
		}
	}
	if run(ctx, opts{from: 100, out: filepath.Join(d, "late")}, []string{mkv}) != 1 {
		t.Error("-from за концом видео должен дать ошибку")
	}

	// повторный запуск по папке ничего не ломает и не плодит файлов
	if run(ctx, opts{}, []string{d}) != 0 {
		t.Fatal("rerun")
	}
	if m, _ := filepath.Glob(filepath.Join(d, "*.part.*")); len(m) > 0 {
		t.Errorf("остались .part: %v", m)
	}
}

func TestPhoto(t *testing.T) {
	if _, err := exec.LookPath("sips"); err != nil {
		t.Skip("нет sips")
	}
	d := t.TempDir()
	sh(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=4000x3000", "-frames:v", "1", filepath.Join(d, "big.png"))
	sh(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=500x400", "-frames:v", "1", filepath.Join(d, "small.png"))
	os.WriteFile(filepath.Join(d, "bad.jpg"), []byte("garbage"), 0o644)

	if run(context.Background(), opts{max: 1000}, []string{d}) != 1 {
		t.Error("битый файл должен дать код 1")
	}
	for f, w := range map[string]string{"big.jpg": "1000", "small.jpg": "500"} {
		out := sh(t, "sips", "-g", "pixelWidth", "-g", "format", filepath.Join(d, f))
		if !strings.Contains(out, "pixelWidth: "+w) || !strings.Contains(out, "format: jpeg") {
			t.Errorf("%s: %s", f, out)
		}
	}
	if _, err := os.Stat(filepath.Join(d, "bad.min.jpg")); err == nil {
		t.Error("для битого файла не должно быть результата")
	}
}

func TestParseTime(t *testing.T) {
	for in, want := range map[string]float64{"": 0, "30m": 1800, "1h20m": 4800, "90": 90, "30:00": 1800, "1:20:00": 4800, "1:02.5": 62.5} {
		if got, err := parseTime(in); err != nil || got != want {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "-5m", "1:2:3:4", "1:-2"} {
		if _, err := parseTime(bad); err == nil {
			t.Errorf("parseTime(%q) должен вернуть ошибку", bad)
		}
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// Что можно класть в MP4 как есть (и что играет в QuickTime/iPhone). Остальное перекодируем.
var mp4Video = set("h264", "hevc", "av1", "vp9", "mpeg4")
var mp4Audio = set("aac", "mp3", "ac3", "eac3", "alac")
var textSubs = set("subrip", "ass", "ssa", "mov_text", "webvtt", "text")

type stream struct {
	Index       int    `json:"index"`
	CodecType   string `json:"codec_type"`
	CodecName   string `json:"codec_name"`
	PixFmt      string `json:"pix_fmt"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
	Tags struct {
		Language string `json:"language"`
	} `json:"tags"`
}

type probe struct {
	Streams []stream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func ffprobe(ctx context.Context, in string) (p probe, err error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", in).Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		return p, fmt.Errorf("ffprobe: %s", strings.TrimSpace(string(ee.Stderr)))
	} else if err != nil {
		return p, err
	}
	return p, json.Unmarshal(out, &p)
}

// pick — какие дорожки типа typ оставить по списку языков. Пустой список — все, none — ни одной.
// Язык сравнивается по началу: ru подходит к rus, en к eng.
func pick(ss []stream, typ, label string, langs []string, warns *[]string) map[int]bool {
	all, match := map[int]bool{}, map[int]bool{}
	for _, s := range ss {
		if s.CodecType != typ {
			continue
		}
		all[s.Index] = true
		for _, l := range langs {
			if strings.HasPrefix(strings.ToLower(s.Tags.Language), l) {
				match[s.Index] = true
			}
		}
	}
	switch {
	case len(langs) == 0:
		return all
	case slices.Contains(langs, "none"):
		return map[int]bool{}
	case len(match) == 0 && len(all) > 0:
		*warns = append(*warns, fmt.Sprintf("нет %s на языке %s — оставлены все", label, strings.Join(langs, ",")))
		return all
	}
	return match
}

// buildArgs решает по каждой дорожке: копировать, перекодировать или пропустить.
func buildArgs(p probe, toMP4 bool, o opts, q int) (args, warns []string) {
	keepA := pick(p.Streams, "audio", "звука", o.audio, &warns)
	keepS := pick(p.Streams, "subtitle", "субтитров", o.subs, &warns)
	n := 0
	for _, s := range p.Streams {
		o2 := strconv.Itoa(n)
		var c []string
		switch s.CodecType {
		case "video":
			if s.Disposition.AttachedPic == 1 {
				continue // обложки
			}
			codec := s.CodecName
			if o.compress || (toMP4 && !mp4Video[codec]) {
				codec = "hevc"
				c = []string{"-c:" + o2, "hevc_videotoolbox", "-q:" + o2, strconv.Itoa(q)}
				if strings.Contains(s.PixFmt, "10") {
					c = append(c, "-profile:"+o2, "main10", "-pix_fmt:"+o2, "p010le")
				}
				if o.res > 0 {
					// По меньшей стороне, чтобы вертикальные видео тоже стали «1080p»; маленькие не увеличиваем.
					r := strconv.Itoa(o.res)
					c = append(c, "-filter:"+o2, "scale=w='if(gt(iw,ih),-2,min(iw,"+r+"))':h='if(gt(iw,ih),min(ih,"+r+"),-2)'")
				}
			} else {
				c = []string{"-c:" + o2, "copy"}
			}
			if toMP4 && codec == "hevc" {
				c = append(c, "-tag:"+o2, "hvc1") // без этого HEVC не играет на Mac/iPhone
			}
		case "audio":
			if !keepA[s.Index] {
				continue
			}
			if toMP4 && !mp4Audio[s.CodecName] {
				c = []string{"-c:" + o2, "aac_at", "-b:" + o2, "256k"}
			} else {
				c = []string{"-c:" + o2, "copy"}
			}
		case "subtitle":
			if !keepS[s.Index] {
				continue
			}
			switch {
			case !toMP4 && s.CodecName == "mov_text":
				c = []string{"-c:" + o2, "srt"}
			case !toMP4:
				c = []string{"-c:" + o2, "copy"}
			case textSubs[s.CodecName]:
				c = []string{"-c:" + o2, "mov_text"}
			default:
				warns = append(warns, fmt.Sprintf("субтитры #%d (%s) не поддерживаются в MP4 — пропущены", s.Index, s.CodecName))
				continue
			}
		default:
			continue // шрифты-вложения, data
		}
		args = append(args, "-map", "0:"+strconv.Itoa(s.Index))
		args = append(args, c...)
		n++
	}
	return args, warns
}

// layout — «отпечаток» устройства файла: склеивать без перекодирования можно только одинаковые.
func layout(p probe) string {
	var b strings.Builder
	for _, s := range p.Streams {
		fmt.Fprintf(&b, "%s/%s/%dx%d ", s.CodecType, s.CodecName, s.Width, s.Height)
	}
	return b.String()
}

func convertVideo(ctx context.Context, o opts, ins []string, part string, toMP4 bool) error {
	var p probe
	var dur float64
	for i, in := range ins {
		pi, err := ffprobe(ctx, in)
		if err != nil {
			return fmt.Errorf("%s: %w", in, err)
		}
		if i == 0 {
			p = pi
		} else if layout(pi) != layout(p) {
			return fmt.Errorf("%s и %s устроены по-разному (кодеки, разрешение или число дорожек) — склеить нельзя", ins[0], in)
		}
		d, _ := strconv.ParseFloat(pi.Format.Duration, 64)
		dur += d
	}
	q := o.q
	if q == 0 {
		q = 60
	}
	args, warns := buildArgs(p, toMP4, o, q)
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "cv: %s: %s\n", ins[0], w)
	}
	if len(args) == 0 {
		return fmt.Errorf("нет дорожек для конвертации")
	}
	inSize := totalSize(ins...)
	if !o.compress { // remux пишет примерно столько же, сколько весит исходник
		var st syscall.Statfs_t
		if syscall.Statfs(filepath.Dir(part), &st) == nil {
			if free := int64(st.Bavail) * int64(st.Bsize); free < inSize {
				return fmt.Errorf("мало места: нужно ~%s, свободно %s", human(inSize), human(free))
			}
		}
	}
	full := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-progress", "pipe:1"}
	if o.from > 0 {
		if dur > 0 && o.from >= dur {
			return fmt.Errorf("-from %s дальше конца видео (%s)", fmtDur(o.from), fmtDur(dur))
		}
		full = append(full, "-ss", ftoa(o.from))
		dur -= o.from
	}
	if o.to > 0 {
		full = append(full, "-t", ftoa(o.to-o.from))
		if dur <= 0 || o.to-o.from < dur {
			dur = o.to - o.from
		}
	}
	if len(ins) > 1 {
		list, err := concatList(ins)
		if err != nil {
			return err
		}
		defer os.Remove(list)
		full = append(full, "-f", "concat", "-safe", "0", "-i", list)
	} else {
		full = append(full, "-i", ins[0])
	}
	full = append(full, args...)
	meta := "0"
	if o.strip {
		meta = "-1" // общие теги файла: место съёмки, дата, устройство; язык дорожек остаётся
	}
	format := "matroska"
	if toMP4 {
		format = "mp4"
	}
	full = append(full, "-map_metadata:g", meta, "-map_chapters", "0", "-f", format, "-y", part)

	cmd := exec.CommandContext(ctx, "ffmpeg", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	showProgress(stdout, dur, filepath.Base(ins[0]))
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %v\n%s", err, strings.TrimSpace(stderr.String()))
	}
	if o.compress && totalSize(part) >= inSize {
		return fmt.Errorf("%w (%s → %s)", errBigger, human(inSize), human(totalSize(part)))
	}
	return nil
}

// concatList — список файлов для склейки в формате ffmpeg concat.
func concatList(ins []string) (string, error) {
	f, err := os.CreateTemp("", "cv-join-*.txt")
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, in := range ins {
		abs, err := filepath.Abs(in)
		if err != nil {
			return f.Name(), err
		}
		fmt.Fprintf(f, "file '%s'\n", strings.ReplaceAll(abs, "'", `'\''`))
	}
	return f.Name(), nil
}

func showProgress(r io.Reader, dur float64, name string) {
	sc := bufio.NewScanner(r)
	var t float64
	speed := ""
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "out_time_us":
			if us, err := strconv.ParseFloat(v, 64); err == nil {
				t = us / 1e6
			}
		case "speed":
			speed = strings.TrimSpace(v)
		case "progress":
			line := name + "  " + fmtDur(t)
			if dur > 0 {
				line = fmt.Sprintf("%s  %3.0f%%", name, min(t/dur*100, 100))
				if sp, err := strconv.ParseFloat(strings.TrimSuffix(speed, "x"), 64); err == nil && sp > 0 {
					line += fmt.Sprintf("  %s  ETA %s", speed, fmtDur((dur-t)/sp))
				}
			}
			fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
		}
	}
	fmt.Fprint(os.Stderr, "\r\033[K")
}

func fmtDur(sec float64) string {
	return time.Duration(max(sec, 0) * float64(time.Second)).Round(time.Second).String()
}

func ftoa(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

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
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
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

// buildArgs решает по каждой дорожке: копировать, перекодировать или пропустить.
func buildArgs(p probe, toMP4, compress bool, q int) (args, warns []string) {
	n := 0
	for _, s := range p.Streams {
		o := strconv.Itoa(n)
		var c []string
		switch s.CodecType {
		case "video":
			if s.Disposition.AttachedPic == 1 {
				continue // обложки
			}
			codec := s.CodecName
			if compress || (toMP4 && !mp4Video[codec]) {
				codec = "hevc"
				c = []string{"-c:" + o, "hevc_videotoolbox", "-q:" + o, strconv.Itoa(q)}
				if strings.Contains(s.PixFmt, "10") {
					c = append(c, "-profile:"+o, "main10", "-pix_fmt:"+o, "p010le")
				}
			} else {
				c = []string{"-c:" + o, "copy"}
			}
			if toMP4 && codec == "hevc" {
				c = append(c, "-tag:"+o, "hvc1") // без этого HEVC не играет на Mac/iPhone
			}
		case "audio":
			if toMP4 && !mp4Audio[s.CodecName] {
				c = []string{"-c:" + o, "aac_at", "-b:" + o, "256k"}
			} else {
				c = []string{"-c:" + o, "copy"}
			}
		case "subtitle":
			switch {
			case !toMP4 && s.CodecName == "mov_text":
				c = []string{"-c:" + o, "srt"}
			case !toMP4:
				c = []string{"-c:" + o, "copy"}
			case textSubs[s.CodecName]:
				c = []string{"-c:" + o, "mov_text"}
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

func convertVideo(ctx context.Context, o opts, in, part string, toMP4 bool) error {
	p, err := ffprobe(ctx, in)
	if err != nil {
		return err
	}
	q := o.q
	if q == 0 {
		q = 60
	}
	args, warns := buildArgs(p, toMP4, o.compress, q)
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "cv: %s: %s\n", in, w)
	}
	if len(args) == 0 {
		return fmt.Errorf("нет дорожек для конвертации")
	}
	if !o.compress { // remux пишет примерно столько же, сколько весит исходник
		fi, err := os.Stat(in)
		var st syscall.Statfs_t
		if err == nil && syscall.Statfs(filepath.Dir(part), &st) == nil {
			if free := st.Bavail * uint64(st.Bsize); free < uint64(fi.Size()) {
				return fmt.Errorf("мало места: нужно ~%s, свободно %s", human(uint64(fi.Size())), human(free))
			}
		}
	}
	format := "matroska"
	if toMP4 {
		format = "mp4"
	}
	dur, _ := strconv.ParseFloat(p.Format.Duration, 64)
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
	full = append(append(full, "-i", in), args...)
	full = append(full, "-map_metadata", "0", "-map_chapters", "0", "-f", format, "-y", part)
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
	showProgress(stdout, dur, filepath.Base(in))
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %v\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
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

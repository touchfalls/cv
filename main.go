// cv — конвертер больших видео (MKV↔MP4, сжатие HEVC) и фото (→ JPEG).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type opts struct {
	compress bool
	q        int
	max      int
	from, to float64 // обрезка видео, секунды; 0 = не задано
	out      string
}

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".mov": true, ".m4v": true}
var photoExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".heif": true, ".webp": true, ".avif": true}

func main() {
	var o opts
	flag.BoolVar(&o.compress, "c", false, "сжать видео в HEVC (аппаратный кодер), результат .mp4")
	flag.IntVar(&o.q, "q", 0, "качество 1..100 (по умолчанию: видео 60, фото 80)")
	flag.IntVar(&o.max, "max", 2560, "фото: максимальная длинная сторона в px, 0 = не уменьшать")
	flag.StringVar(&o.out, "o", "", "папка для результатов (по умолчанию рядом с исходником)")
	from := flag.String("from", "", "видео: начать с этого момента (30m, 1h20m, 30:00, 1:20:00)")
	to := flag.String("to", "", "видео: закончить на этом моменте (тот же формат)")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `Использование: cv [флаги] файлы или папки...

  cv film.mkv       → film.mp4 (без перекодирования, быстро)
  cv film.mp4       → film.mkv
  cv -c film.mkv    → сжатый film.mp4 (HEVC)
  cv -to 30m film.mkv          → film.cut.mp4, первые 30 минут
  cv -from 1h -to 1h30m film.mkv → film.cut.mp4, с 1:00:00 до 1:30:00
  cv ./photos       → JPEG ≤2560px рядом с оригиналами

Флаги ставятся перед файлами.
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	var err1, err2 error
	o.from, err1 = parseTime(*from)
	o.to, err2 = parseTime(*to)
	if err := errors.Join(err1, err2); err != nil {
		fmt.Fprintln(os.Stderr, "cv:", err)
		os.Exit(2)
	}
	if o.to > 0 && o.to <= o.from {
		fmt.Fprintln(os.Stderr, "cv: -to должно быть больше -from")
		os.Exit(2)
	}
	if o.q < 0 || o.q > 100 || o.max < 0 {
		fmt.Fprintln(os.Stderr, "cv: -q должно быть 1..100, -max ≥ 0")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, o, flag.Args()))
}

type job struct{ in, out string }

// run возвращает код выхода: 0 — всё ок, 1 — были ошибки.
func run(ctx context.Context, o opts, args []string) int {
	failed := 0
	if o.out != "" {
		if err := os.MkdirAll(o.out, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "cv:", err)
			return 1
		}
	}
	var videos, photos []job
	for _, a := range args {
		err := filepath.WalkDir(a, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			inside := p != a
			name := d.Name()
			// В папках пропускаем скрытое (._file на внешних дисках) и наши недописанные .part.
			if inside && (strings.HasPrefix(name, ".") || strings.Contains(name, ".part.")) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			switch {
			case videoExt[ext]:
				to := ".mkv"
				if o.from > 0 || o.to > 0 {
					to = ".cut" + to // не путаем с полной конвертацией film.mp4
				}
				if o.compress || ext == ".mkv" {
					to = strings.TrimSuffix(to, ".mkv") + ".mp4"
				}
				videos = append(videos, job{p, outPath(p, to, o.out)})
			case photoExt[ext]:
				photos = append(photos, job{p, outPath(p, ".jpg", o.out)})
			case !inside:
				fmt.Fprintf(os.Stderr, "cv: %s: неизвестный формат\n", p)
				failed++
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "cv:", err)
			failed++
		}
	}
	// Не конвертируем файлы, которые сами являются результатом другого файла (photo.heic → photo.jpg).
	planned := map[string]bool{}
	for _, j := range append(videos, photos...) {
		planned[key(j.out)] = true
	}
	drop := func(js []job) (r []job) {
		for _, j := range js {
			if !planned[key(j.in)] {
				r = append(r, j)
			}
		}
		return r
	}
	videos, photos = drop(videos), drop(photos)

	for _, j := range videos {
		if ctx.Err() != nil {
			break
		}
		toMP4 := strings.HasSuffix(j.out, ".mp4")
		if err := process(j, func(part string) error { return convertVideo(ctx, o, j.in, part, toMP4) }); err != nil {
			fmt.Fprintf(os.Stderr, "cv: %s: %v\n", j.in, err)
			failed++
		}
	}

	var pfailed atomic.Int32
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for _, j := range photos {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := process(j, func(part string) error { return convertPhoto(ctx, o, j.in, part) }); err != nil {
				fmt.Fprintf(os.Stderr, "cv: %s: %v\n", j.in, err)
				pfailed.Add(1)
			}
		})
	}
	wg.Wait()
	failed += int(pfailed.Load())
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "cv: прервано")
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// key — путь для сравнения: APFS по умолчанию не различает регистр (photo.JPG == photo.jpg).
func key(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		a = p
	}
	return strings.ToLower(a)
}

func outPath(in, ext, dir string) string {
	if dir == "" {
		dir = filepath.Dir(in)
	}
	base := strings.TrimSuffix(filepath.Base(in), filepath.Ext(in))
	out := filepath.Join(dir, base+ext)
	if key(out) == key(in) {
		out = filepath.Join(dir, base+".min"+ext)
	}
	return out
}

// process пишет во временный файл и переименовывает только при успехе — битых результатов не остаётся.
func process(j job, conv func(part string) error) error {
	if _, err := os.Stat(j.out); err == nil {
		fmt.Printf("пропуск %s: %s уже есть\n", j.in, j.out)
		return nil
	}
	ext := filepath.Ext(j.out)
	part := strings.TrimSuffix(j.out, ext) + ".part" + ext
	if err := conv(part); err != nil {
		os.Remove(part)
		return err
	}
	if _, err := os.Stat(part); err != nil {
		return fmt.Errorf("результат не создан")
	}
	if err := os.Rename(part, j.out); err != nil {
		os.Remove(part)
		return err
	}
	fmt.Printf("%s → %s  (%s → %s)\n", j.in, j.out, size(j.in), size(j.out))
	return nil
}

func size(p string) string {
	fi, err := os.Stat(p)
	if err != nil {
		return "?"
	}
	return human(uint64(fi.Size()))
}

func human(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.0f КБ", float64(b)/(1<<10))
	}
}

// parseTime понимает 30m / 1h20m / 90s, а также 30:00, 1:20:00 и просто секунды.
func parseTime(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d.Seconds(), nil
	}
	parts := strings.Split(s, ":")
	var sec float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 || len(parts) > 3 {
			return 0, fmt.Errorf("неверное время %q (примеры: 30m, 1h20m, 30:00, 1:20:00)", s)
		}
		sec = sec*60 + v
	}
	return sec, nil
}

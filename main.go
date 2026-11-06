// cv — конвертер больших видео (MKV⇄MP4, сжатие, обрезка, склейка) и фото.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "dev" // подставляется при сборке релиза: -ldflags "-X main.version=..."

type opts struct {
	compress, strip, join, version bool
	q, max, res                    int
	from, to                       float64 // обрезка видео, секунды; 0 = не задано
	out, format                    string  // format — формат фото: jpg, heic, avif, webp
	audio, subs                    []string
}

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".mov": true, ".m4v": true}
var photoExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".heif": true, ".webp": true, ".avif": true}
var photoOut = map[string]string{"jpg": ".jpg", "heic": ".heic", "avif": ".avif", "webp": ".webp"}

var errUsage = errors.New("usage") // ошибка уже напечатана вместе с подсказкой

func main() {
	o, files, err := parseFlags(os.Args[1:])
	switch {
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "cv:", err)
		os.Exit(2)
	case o.version:
		fmt.Println("cv", version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, o, files))
}

func parseFlags(args []string) (o opts, files []string, err error) {
	fs := flag.NewFlagSet("cv", flag.ContinueOnError)
	fs.BoolVar(&o.compress, "c", false, "сжать видео в HEVC (аппаратный кодер), результат .mp4")
	fs.IntVar(&o.q, "q", 0, "качество 1..100 (по умолчанию: видео 60, фото 80)")
	fs.IntVar(&o.res, "res", 0, "видео: уменьшить до 1080, 720… по меньшей стороне (включает сжатие)")
	from := fs.String("from", "", "видео: начать с этого момента (30m, 1h20m, 30:00, 1:20:00)")
	to := fs.String("to", "", "видео: закончить на этом моменте (тот же формат)")
	audio := fs.String("audio", "", "видео: оставить звук только на этих языках: rus или rus,eng; none — без звука")
	subs := fs.String("subs", "", "видео: то же для субтитров; none — убрать все")
	fs.BoolVar(&o.join, "join", false, "видео: склеить файлы в один в порядке перечисления")
	fs.StringVar(&o.format, "f", "jpg", "формат фото: jpg, heic, avif, webp")
	fs.IntVar(&o.max, "max", 2560, "фото: максимальная длинная сторона в px, 0 = не уменьшать")
	fs.BoolVar(&o.strip, "strip", false, "удалить GPS, модель камеры и прочие метаданные")
	fs.StringVar(&o.out, "o", "", "папка для результатов (по умолчанию рядом с исходником)")
	fs.BoolVar(&o.version, "version", false, "показать версию")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Использование: cv [флаги] файлы или папки...

  cv film.mkv                    → film.mp4 (без перекодирования, быстро)
  cv film.mp4                    → film.mkv
  cv -c film.mkv                 → сжатый film.mp4 (HEVC)
  cv -res 1080 film.mkv          → сжатый film.mp4 в 1080p
  cv -to 30m film.mkv            → film.cut.mp4, первые 30 минут
  cv -audio rus -subs none film.mkv → только русская озвучка, без субтитров
  cv -join 1.mkv 2.mkv 3.mkv     → 1.joined.mkv
  cv ./photos                    → JPEG ≤2560px рядом с оригиналами
  cv -f webp -strip ./photos     → WebP без GPS

`)
		fs.PrintDefaults()
	}
	// Флаги можно писать и после файлов: cv film.mkv -c.
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return o, nil, err
			}
			return o, nil, errUsage
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if o.version {
		return o, nil, nil
	}
	if len(files) == 0 {
		fs.Usage()
		return o, nil, errUsage
	}
	var err1, err2 error
	o.from, err1 = parseTime(*from)
	o.to, err2 = parseTime(*to)
	switch {
	case err1 != nil || err2 != nil:
		return o, nil, errors.Join(err1, err2)
	case o.to > 0 && o.to <= o.from:
		return o, nil, errors.New("-to должно быть больше -from")
	case o.q < 0 || o.q > 100:
		return o, nil, errors.New("-q должно быть от 1 до 100")
	case o.max < 0 || o.res < 0:
		return o, nil, errors.New("-max и -res не могут быть отрицательными")
	case photoOut[o.format] == "":
		return o, nil, fmt.Errorf("неизвестный формат фото %q: можно jpg, heic, avif, webp", o.format)
	}
	o.audio, o.subs = splitList(*audio), splitList(*subs)
	return o, files, nil
}

func splitList(s string) (r []string) {
	for _, x := range strings.Split(strings.ToLower(s), ",") {
		if x = strings.TrimSpace(x); x != "" {
			r = append(r, x)
		}
	}
	return r
}

type job struct {
	ins []string // несколько — только при склейке
	out string
}

// stats считает результаты для итоговой строки; фото обрабатываются параллельно, поэтому под мьютексом.
type stats struct {
	mu                    sync.Mutex
	done, skipped, failed int
	in, out               int64
}

func (s *stats) fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "cv: "+format+"\n", a...)
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

// run возвращает код выхода: 0 — всё ок, 1 — были ошибки.
func run(ctx context.Context, o opts, args []string) int {
	var st stats
	if o.res > 0 {
		o.compress = true // уменьшить разрешение без перекодирования нельзя
	}
	if o.format == "" {
		o.format = "jpg"
	}
	if o.out != "" {
		if err := os.MkdirAll(o.out, 0o755); err != nil {
			st.fail("%v", err)
			return 1
		}
	}
	var videos, photos []string
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
			switch ext := strings.ToLower(filepath.Ext(p)); {
			case videoExt[ext]:
				videos = append(videos, p)
			case photoExt[ext]:
				photos = append(photos, p)
			case !inside:
				st.fail("%s: неизвестный формат", p)
			}
			return nil
		})
		if err != nil {
			st.fail("%v", err)
		}
	}

	var vjobs, pjobs []job
	if o.join {
		switch {
		case len(photos) > 0:
			st.fail("-join склеивает только видео")
			return 1
		case len(videos) < 2:
			st.fail("для -join нужно хотя бы два видео")
			return 1
		}
		vjobs = []job{{videos, outPath(videos[0], videoTarget(videos[0], o), o.out)}}
	} else {
		for _, v := range videos {
			vjobs = append(vjobs, job{[]string{v}, outPath(v, videoTarget(v, o), o.out)})
		}
	}
	for _, p := range photos {
		pjobs = append(pjobs, job{[]string{p}, outPath(p, photoOut[o.format], o.out)})
	}
	if len(pjobs) > 0 && o.format == "webp" {
		if _, err := exec.LookPath("cwebp"); err != nil {
			st.fail("для WebP нужен cwebp: brew install webp")
			return 1
		}
	}
	// Не конвертируем файлы, которые сами являются результатом другого файла (photo.heic → photo.jpg).
	planned := map[string]bool{}
	for _, j := range append(vjobs, pjobs...) {
		planned[key(j.out)] = true
	}
	drop := func(js []job) (r []job) {
		for _, j := range js {
			if len(j.ins) > 1 || !planned[key(j.ins[0])] {
				r = append(r, j)
			}
		}
		return r
	}
	vjobs, pjobs = drop(vjobs), drop(pjobs)

	for _, j := range vjobs {
		if ctx.Err() != nil {
			break
		}
		toMP4 := strings.HasSuffix(j.out, ".mp4")
		st.process(j, func(part string) error { return convertVideo(ctx, o, j.ins, part, toMP4) })
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for _, j := range pjobs {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			st.process(j, func(part string) error { return convertPhoto(ctx, o, j.ins[0], part) })
		})
	}
	wg.Wait()
	st.summary()
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "cv: прервано")
		return 1
	}
	if st.failed > 0 {
		return 1
	}
	return 0
}

// videoTarget — расширение результата: MKV⇄MP4; склейка сохраняет контейнер; сжатие всегда в MP4.
func videoTarget(in string, o opts) string {
	ext := strings.ToLower(filepath.Ext(in))
	to := ".mkv"
	if o.compress || (o.join && ext != ".mkv") || (!o.join && ext == ".mkv") {
		to = ".mp4"
	}
	if o.from > 0 || o.to > 0 {
		to = ".cut" + to // не путаем с полной конвертацией film.mp4
	}
	if o.join {
		to = ".joined" + to
	}
	return to
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

var errBigger = errors.New("сжатый файл получился больше оригинала — оставлен оригинал, попробуйте -q поменьше")

// process пишет во временный файл и переименовывает только при успехе — битых результатов не остаётся.
func (s *stats) process(j job, conv func(part string) error) {
	in := j.ins[0]
	if _, err := os.Stat(j.out); err == nil {
		fmt.Printf("пропуск %s: %s уже есть\n", in, j.out)
		s.mu.Lock()
		s.skipped++
		s.mu.Unlock()
		return
	}
	ext := filepath.Ext(j.out)
	part := strings.TrimSuffix(j.out, ext) + ".part" + ext
	err := conv(part)
	if err == nil {
		if _, e := os.Stat(part); e != nil {
			err = errors.New("результат не создан")
		}
	}
	if err == nil {
		err = os.Rename(part, j.out)
	}
	if err != nil {
		os.Remove(part)
		if errors.Is(err, errBigger) {
			fmt.Printf("%s: %v\n", in, err)
			s.mu.Lock()
			s.skipped++
			s.mu.Unlock()
			return
		}
		s.fail("%s: %v", in, err)
		return
	}
	inSize, outSize := totalSize(j.ins...), totalSize(j.out)
	fmt.Printf("%s → %s  (%s → %s)\n", in, j.out, human(inSize), human(outSize))
	s.mu.Lock()
	s.done++
	s.in += inSize
	s.out += outSize
	s.mu.Unlock()
}

func (s *stats) summary() {
	if s.done+s.skipped+s.failed < 2 {
		return
	}
	line := fmt.Sprintf("\nГотово: %d", s.done)
	if s.in > 0 {
		line += fmt.Sprintf(", %s → %s", human(s.in), human(s.out))
		if s.out < s.in {
			line += fmt.Sprintf(" (−%d%%)", 100-s.out*100/s.in)
		}
	}
	if s.skipped > 0 {
		line += fmt.Sprintf(", пропущено %d", s.skipped)
	}
	if s.failed > 0 {
		line += fmt.Sprintf(", ошибок %d", s.failed)
	}
	fmt.Println(line)
}

func totalSize(ps ...string) (n int64) {
	for _, p := range ps {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func human(b int64) string {
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

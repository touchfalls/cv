package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Как повернуть пиксели по EXIF-ориентации (для WebP: cwebp её не понимает). sips -r — по часовой.
var orientOps = map[int][][]string{
	2: {{"-f", "horizontal"}},
	3: {{"-r", "180"}},
	4: {{"-f", "vertical"}},
	5: {{"-r", "90"}, {"-f", "horizontal"}},
	6: {{"-r", "90"}},
	7: {{"-r", "270"}, {"-f", "horizontal"}},
	8: {{"-r", "270"}},
}

// convertPhoto использует системный sips: читает HEIC/WebP/AVIF/PNG/JPEG, пишет JPEG/HEIC/AVIF.
// WebP пишет cwebp (brew install webp).
func convertPhoto(ctx context.Context, o opts, in, part string) error {
	q := o.q
	if q == 0 {
		q = 80
	}
	qs := strconv.Itoa(q)
	var resize []string
	if o.max > 0 {
		w, h, err := dims(ctx, in)
		if err != nil {
			return err
		}
		if max(w, h) > o.max { // sips -Z ещё и увеличивает маленькие, поэтому проверяем сами
			resize = []string{"-Z", strconv.Itoa(o.max)}
		}
	}
	tmp := part + ".tmp"
	defer os.Remove(tmp)
	switch o.format {
	case "jpg":
		if err := sips(ctx, "-s", "format", "jpeg", "-s", "formatOptions", qs, resize, in, "--out", part); err != nil {
			return err
		}
		if o.strip {
			if err := stripFile(part); err != nil {
				return err
			}
		}
	case "heic", "avif":
		src := in
		if o.strip {
			// shortcut: метаданные чистим через промежуточный JPEG q100 — теряется прозрачность PNG;
			// если понадобится, чистить EXIF прямо внутри HEIC/AVIF.
			if err := sips(ctx, "-s", "format", "jpeg", "-s", "formatOptions", "100", resize, in, "--out", tmp); err != nil {
				return err
			}
			if err := stripFile(tmp); err != nil {
				return err
			}
			src, resize = tmp, nil
		}
		if err := sips(ctx, "-s", "format", o.format, "-s", "formatOptions", qs, resize, src, "--out", part); err != nil {
			return err
		}
	case "webp":
		// TIFF без потерь и с прозрачностью; поворот по EXIF применяем к пикселям сами.
		if err := sips(ctx, "-s", "format", "tiff", resize, in, "--out", tmp); err != nil {
			return err
		}
		b, err := os.ReadFile(tmp)
		if err != nil {
			return err
		}
		for _, op := range orientOps[tiffOrientation(b)] {
			if err := sips(ctx, op, tmp); err != nil {
				return err
			}
		}
		// -metadata icc: EXIF не переносим вовсе, так что WebP всегда без GPS.
		if out, err := exec.CommandContext(ctx, "cwebp", "-quiet", "-q", qs, "-metadata", "icc", tmp, "-o", part).CombinedOutput(); err != nil {
			return fmt.Errorf("cwebp: %v: %s", err, strings.TrimSpace(string(out)))
		}
	default:
		return fmt.Errorf("неизвестный формат фото %q", o.format)
	}
	return checkBigger(o, in, part)
}

// checkBigger: пересжатие в тот же формат не должно увеличивать файл. Смена формата (HEIC → JPEG) — ради
// совместимости, там больший размер нормален.
func checkBigger(o opts, in, part string) error {
	norm := map[string]string{".jpeg": ".jpg", ".heif": ".heic"}
	ext := strings.ToLower(filepath.Ext(in))
	if n, ok := norm[ext]; ok {
		ext = n
	}
	if ext != photoOut[o.format] || totalSize(part) < totalSize(in) {
		return nil
	}
	if o.strip && ext == ".jpg" { // хотя бы почистим оригинал от GPS, без пересжатия
		b, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		s, err := stripJPEG(b)
		if err != nil {
			return err
		}
		return os.WriteFile(part, s, 0o644)
	}
	return fmt.Errorf("%w (%s → %s)", errBigger, human(totalSize(in)), human(totalSize(part)))
}

func stripFile(p string) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	s, err := stripJPEG(b)
	if err != nil {
		return err
	}
	return os.WriteFile(p, s, 0o644)
}

// sips принимает строки и срезы строк вперемешку — так короче собирать аргументы.
func sips(ctx context.Context, parts ...any) error {
	var args []string
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			args = append(args, v)
		case []string:
			args = append(args, v...)
		}
	}
	if out, err := exec.CommandContext(ctx, "sips", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("sips: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func dims(ctx context.Context, in string) (w, h int, err error) {
	out, err := exec.CommandContext(ctx, "sips", "-g", "pixelWidth", "-g", "pixelHeight", in).CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("sips: %v: %s", err, strings.TrimSpace(string(out)))
	}
	f := strings.Fields(string(out))
	for i := 0; i+1 < len(f); i++ {
		switch f[i] {
		case "pixelWidth:":
			w, _ = strconv.Atoi(f[i+1])
		case "pixelHeight:":
			h, _ = strconv.Atoi(f[i+1])
		}
	}
	if w == 0 || h == 0 {
		return 0, 0, fmt.Errorf("не удалось прочитать изображение")
	}
	return w, h, nil
}

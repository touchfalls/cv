package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// convertPhoto использует системный sips: читает HEIC/WebP/AVIF/PNG/JPEG без зависимостей.
func convertPhoto(ctx context.Context, o opts, in, part string) error {
	q := o.q
	if q == 0 {
		q = 80
	}
	args := []string{"-s", "format", "jpeg", "-s", "formatOptions", strconv.Itoa(q)}
	if o.max > 0 {
		w, h, err := dims(ctx, in)
		if err != nil {
			return err
		}
		if max(w, h) > o.max { // sips -Z ещё и увеличивает маленькие, поэтому проверяем сами
			args = append(args, "-Z", strconv.Itoa(o.max))
		}
	}
	args = append(args, in, "--out", part)
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

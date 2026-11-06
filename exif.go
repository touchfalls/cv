package main

import (
	"bytes"
	"encoding/binary"
	"errors"
)

var errBadJPEG = errors.New("повреждённый JPEG")

// stripJPEG убирает из JPEG EXIF (GPS, модель камеры, дату), XMP, IPTC и комментарии без пересжатия.
// Ориентацию сохраняет, иначе вертикальные фото лягут на бок. Цветовой профиль (ICC) тоже остаётся.
func stripJPEG(b []byte) ([]byte, error) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, errBadJPEG
	}
	var app0, kept [][]byte
	orient := 0
	i := 2
	for {
		if i+4 > len(b) || b[i] != 0xFF {
			return nil, errBadJPEG
		}
		m := b[i+1]
		if m == 0xFF { // байты-заполнители
			i++
			continue
		}
		if m == 0xDA { // начало сжатых данных — дальше копируем как есть
			break
		}
		n := int(b[i+2])<<8 | int(b[i+3])
		if n < 2 || i+2+n > len(b) {
			return nil, errBadJPEG
		}
		seg, payload := b[i:i+2+n], b[i+4:i+2+n]
		switch {
		case m == 0xE1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")):
			orient = tiffOrientation(payload[6:])
		case m == 0xE1, m == 0xED, m == 0xFE: // XMP, IPTC, комментарий
		case m == 0xE0:
			app0 = append(app0, seg)
		default:
			kept = append(kept, seg)
		}
		i += 2 + n
	}
	out := []byte{0xFF, 0xD8}
	for _, s := range app0 {
		out = append(out, s...)
	}
	if orient > 1 {
		out = append(out, orientationExif(orient)...)
	}
	for _, s := range kept {
		out = append(out, s...)
	}
	return append(out, b[i:]...), nil
}

// tiffOrientation читает тег Orientation (0x0112) из IFD0 TIFF-структуры (EXIF внутри JPEG или файл .tiff).
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	off := int(bo.Uint32(t[4:]))
	if off < 8 || off+2 > len(t) {
		return 0
	}
	for k := range int(bo.Uint16(t[off:])) {
		e := off + 2 + 12*k
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			return int(bo.Uint16(t[e+8:]))
		}
	}
	return 0
}

// orientationExif — минимальный сегмент APP1 с одним тегом Orientation.
func orientationExif(o int) []byte {
	p := []byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08" + // заголовок TIFF, IFD0 по смещению 8
		"\x00\x01" + // одна запись
		"\x01\x12\x00\x03\x00\x00\x00\x01") // Orientation, SHORT, 1 значение
	p = append(p, 0, byte(o), 0, 0, 0, 0, 0, 0) // значение + «следующего IFD нет»
	return append([]byte{0xFF, 0xE1, byte((len(p) + 2) >> 8), byte(len(p) + 2)}, p...)
}

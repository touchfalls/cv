#!/bin/sh
# Выпуск новой версии: ./release.sh 0.2.0
# Делает тег, GitHub Release с готовыми программами и обновляет формулу в touchfalls/homebrew-tap.
set -eu
v=${1:?укажите версию, например: ./release.sh 0.2.0}
tag="v$v"
repo=touchfalls/cv

[ -z "$(git status --porcelain)" ] || { echo "Сначала закоммитьте изменения."; exit 1; }
go vet ./...
go test ./...

git tag -a "$tag" -m "$tag"
git push origin main "$tag"

dist=$(mktemp -d)
trap 'rm -rf "$dist"' EXIT
for arch in arm64 amd64; do
  GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$v" -o "$dist/cv" .
  tar -czf "$dist/cv-$v-darwin-$arch.tar.gz" -C "$dist" cv
done
gh release create "$tag" "$dist"/cv-*.tar.gz --repo "$repo" --title "$tag" --generate-notes

sha=$(curl -fsSL "https://github.com/$repo/archive/refs/tags/$tag.tar.gz" | shasum -a 256 | cut -d' ' -f1)
git clone -q "https://github.com/touchfalls/homebrew-tap" "$dist/tap"
mkdir -p "$dist/tap/Formula"
sed -e "/^# Шаблон/d" -e "s/@VERSION@/$v/g" -e "s/@SHA256@/$sha/" packaging/cv.rb > "$dist/tap/Formula/cv.rb"
git -C "$dist/tap" add Formula/cv.rb
git -C "$dist/tap" commit -q -m "cv $v"
git -C "$dist/tap" push -q origin HEAD:main

echo "Готово: $tag. Обновить у себя: brew update && brew upgrade cv"

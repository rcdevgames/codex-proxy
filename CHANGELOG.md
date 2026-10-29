# Changelog

## 2026-10-29 — endpoint edit gambar

Tambah `POST /v1/images/edits`:

- JSON native `images: [{"image_url":"data:image/...;base64,..."}]` diteruskan ke upstream `/images/edits`.
- `multipart/form-data` juga diterima (`image`, `image[]`, `images`, `mask`), diubah menjadi data URL di gateway.
- Validasi lokal: prompt wajib ≤4000 karakter, `n=1`, 1–16 gambar input, body ≤32MB, gambar ≤50MB.
- `postImage` sekarang menerima path endpoint; route generations tidak berubah perilakunya.
- Unit test baru untuk validasi, forwarding, multipart, dan penolakan non-gambar.
- README: bagian "Gambar" dengan contoh `curl -F`.

Verifikasi live: edit JSON merah→biru (piksel tengah `(0,0,252)`) dan multipart merah→lingkaran hijau (`(2,195,3)`), keduanya HTTP 200 lewat gateway; guard tanpa `images` mengembalikan 400 lokal.

Detail rencana dan sisa pending: `docs/plan-2026-10-29-image-edits.md`.

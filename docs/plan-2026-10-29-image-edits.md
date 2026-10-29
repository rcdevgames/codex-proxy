# Rencana: endpoint edit gambar di codex-bridge

Tanggal: 2026-10-29
Status: selesai (terverifikasi live)

## Tujuan

Mas bos bisa mengirim gambar ke codex-bridge untuk diedit (ganti warna objek, ganti background, dan sejenisnya), bukan hanya generate gambar baru.

## Temuan sebelum implementasi

1. Upstream `{upstream}/images/generations` menerima JSON dan mengembalikan `data[].b64_json` — sudah dipakai gateway.
2. Upstream juga punya `{upstream}/images/edits`, tetapi **menolak multipart** (`400 Unsupported content type`) dan meminta JSON dengan bentuk:

   ```json
   {"model":"gpt-image-1-mini","prompt":"...","size":"1024x1024",
    "images":[{"image_url":"data:image/png;base64,..."}]}
   ```

   Ini dibuktikan dengan probe langsung ke upstream memakai token `auth.json`; bentuk `image` (string), `images` (array of string), `image_urls`, dan `input` semuanya ditolak sebelum akhirnya `images[].image_url` diterima HTTP 200.
3. Gateway sebelumnya hanya punya route `POST /v1/images/generations`, sehingga klien tidak punya jalur edit.

## Rencana implementasi

- [x] Tambah route `POST /v1/images/edits`.
- [x] Pisahkan decode/validate/proxy pada `images.go` tanpa mengubah perilaku `/images/generations`.
- [x] Validasi lokal: prompt (wajib, ≤4000 karakter), `n=1`, `images` wajib 1–16 objek dengan `image_url` data URL; body ≤32MB; gambar ≤50MB.
- [x] Terima `multipart/form-data` untuk kenyamanan klien: field `image`/`image[]`/`images`/`mask` diubah menjadi data URL, field numerik (`n`, `output_compression`) dikonversi ke angka.
- [x] `postImage` menerima path endpoint agar satu fungsi melayani generations dan edits.
- [x] Unit test: validasi lokal, forwarding JSON ke path `/images/edits`, konversi multipart, penolakan unggahan non-gambar, key wajib.
- [x] Dokumentasi README + changelog.

## Verifikasi

- `go vet ./...` dan `go test -race ./...` → `ok codex-bridge`.
- Live lewat gateway (PM2, port 8765):
  - JSON: gambar kotak merah → edit "jadi biru" → HTTP 200, piksel tengah `(0,0,252)` (biru).
  - Multipart: kotak merah → "ganti dengan lingkaran hijau" → HTTP 200, piksel tengah `(2,195,3)` (hijau), background putih bersih (dicek visual).
  - Guard: body tanpa `images` → `400 {"message":"images is required"}` tanpa menyentuh upstream.
- Log gateway mencatat `[images] ok endpoint=/images/edits status=200` untuk kedua jalur.

## Sisa / pending

- Dukungan `mask` sudah diteruskan sebagai `{"image_url":...}` tetapi belum diuji ke upstream (perlu gambar + mask nyata).
- `n>1` masih ditolak oleh cost guard lokal (`n=1`), sama seperti generations.
- Endpoint gambar belum masuk statistik monitor usage (hanya token teks yang dihitung).
- Model gambar yang valid mengikuti akun; belum ada endpoint `/v1/models` khusus gambar.

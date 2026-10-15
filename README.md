# codex-bridge

Gateway Go lokal yang membaca `auth.json` Hermes (format aslinya), tanpa login Codex CLI terpisah. Konfigurasi gateway adalah YAML; berkas OAuth tetap JSON.

## Berkas

`config.yaml` (izin `0600`) hanya memuat:

```yaml
host: 127.0.0.1
port: 8765
api_keys:
  - <apikey-pertama-minimal-16-karakter>
  - <apikey-kedua-minimal-16-karakter>
refresh_check_seconds: 60
```

Di instalasi ini `config.yaml` sudah berisi key lama yang dimigrasikan dari `config.json`, tanpa perubahan nilainya. `auth.json` **harus** ada di direktori `config.yaml` atau direktori binary (lokasi config didahulukan); tidak ada opsi `auth_file`. Di sini `auth.json` pada folder proyek berupa symlink ke file autentikasi Hermes yang sama, sehingga tidak ada salinan refresh token yang bisa berbeda/ter-rotasi terpisah. Gateway me-resolve symlink sebelum mengunci dan menulis ulang file OAuth tersebut. Jangan menghapus target symlink di profil Hermes. Jika dua lokasi sama-sama punya `auth.json`, berkas di sebelah `config.yaml` digunakan. `upstream_url` ditetapkan di kode, bukan dalam YAML.

```bash
cd /home/unity008/.workspace/codex-bridge
go test -race ./...
go build -o codex-bridge .
./codex-bridge -config config.yaml
```

Parameter `-config` default-nya `config.yaml` dari working directory. Server hanya boleh bind ke loopback; untuk akses jarak jauh diperlukan reverse proxy TLS tersendiri. Jaga `config.yaml` pada izin `0600`; jangan commit `config.yaml` atau `auth.json`.

Semua endpoint memakai header `Authorization: Bearer <salah-satu-api_keys>`. Contoh:

```bash
curl -H 'Authorization: Bearer <apikey>' http://127.0.0.1:8765/usages
curl -H 'Authorization: Bearer <apikey>' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-sol","messages":[{"role":"user","content":"Hello"}]}' \
  http://127.0.0.1:8765/v1/chat/completions
```

Endpoint: `GET /usages` atau `/v1/usages` (plan, sisa kuota per jendela, waktu reset UTC, reset tersimpan), `GET /v1/models` (daftar model dari upstream, lihat bawah), `POST /v1/chat/completions` (teks, streaming/nonstreaming), `POST /v1/responses` (SSE Responses untuk klien Codex). Semua endpoint memerlukan API key gateway. `stream` di `/v1/responses` dipaksa `true` karena upstream Codex adalah SSE; klien yang meminta JSON nonstreaming untuk endpoint itu tidak didukung. Input tools pada `/v1/responses` diteruskan apa adanya; terjemahan tools untuk chat completions belum tersedia. Format Chat Completions masih subset, bukan semua parameter [OI].

## Model

Daftar model diambil dari upstream (`GET {upstream}/models?client_version=...`) memakai access token akun yang sama, di-cache 10 menit, dan disajikan sebagai `{"object":"list","data":[{"id":"...","object":"model"}]}`. Entri dengan `visibility: hide` (internal CLI) tidak diiklankan. Kalau upstream tidak bisa dihubungi, endpoint mengembalikan satu model cadangan (`gpt-6-sol`) dan mencatat alasannya di log — daftar statis tidak dipakai karena model yang tidak ada di akun akan ditolak upstream.

Model yang tersedia ditentukan oleh paket akun, bukan oleh gateway: nama model Codex yang lama/umum (mis. `gpt-5-codex`, `gpt-5`, `o3`, `gpt-4o`) dijawab upstream dengan `400 ... is not supported when using Codex with a ChatGPT account`. Karena itu pakai id dari `/v1/models`, jangan asumsi dari dokumentasi Codex CLI.

Pesan error upstream yang dikembalikan ke klien hanya berisi status HTTP dan kolom alasan (`detail` / `error.message`), dipotong 300 karakter; body lain tetap disensor dan hanya dicatat sebagai jumlah byte di log.

Contoh konfigurasi Codex CLI sebagai klien (TOML milik Codex CLI, *berbeda* dengan JSON konfigurasi gateway):

```toml
model = "gpt-6-sol"
model_provider = "local_bridge"

[model_providers.local_bridge]
name = "Local Codex Bridge"
base_url = "http://127.0.0.1:8765/v1"
wire_api = "responses"
env_key = "CODEX_BRIDGE_API_KEY"
```

Set `CODEX_BRIDGE_API_KEY` di shell klien ke salah satu key dari `config.yaml`; jangan tulis key langsung di TOML. Konfigurasi ini berhasil diuji end-to-end dengan `codex exec` (provider `local_bridge`, model `gpt-6-sol`, respons `OK`, exit 0). Itu membuktikan jalur dasar, bukan seluruh fitur CLI. Autentikasi gateway memakai key YAML; autentikasi upstream memakai `auth.json` Hermes.

## Menjalankan lewat PM2

`ecosystem.config.cjs` sudah ada di direktori ini. Binary Go wajib dijalankan dengan `interpreter: 'none'`; tanpa itu PM2 membungkusnya dengan Node dan proses langsung mati dengan `SyntaxError`.

```bash
pm2 start ecosystem.config.cjs     # atau: pm2 start codex-bridge
pm2 save                           # supaya ikut naik setelah reboot
pm2 logs codex-bridge
```

Perubahan pada `args`/`env`/`interpreter` **tidak** terbaca oleh `pm2 restart`; pakai `pm2 delete codex-bridge && pm2 start ecosystem.config.cjs`.

`auth.json` harus berupa berkas biasa (bukan symlink rusak) di samping `config.yaml`; gateway me-resolve symlink lebih dulu, jadi symlink ke target yang tidak ada akan ditolak dengan `auth.json must be alongside config.yaml or the binary`. Izin yang dipakai: `config.yaml` `0600`, `auth.json` `0600`.

## Akses publik

Reverse proxy TLS (Caddy/FrankenPHP di host yang sama) → `reverse_proxy 127.0.0.1:8765`, TLS Let's Encrypt, HTTP→HTTPS 308, dan respons SSE di-*flush* tanpa buffering. Autentikasi tetap key gateway (`Authorization: Bearer`), bukan kredensial upstream.

Host block-nya ada di `/etc/frankenphp/Caddyfile`. Perubahan dimuat lewat admin API (`POST http://127.0.0.1:2019/load`, `Content-Type: text/caddyfile`) karena `systemctl reload frankenphp` rusak di host ini (`226/NAMESPACE`).

## Batasan yang diketahui

`POST /v1/responses` meneruskan `input` apa adanya. Bentuk array (`[{"role":"user","content":"..."}]` maupun `content` berupa array `input_text`) diterima upstream; `input` berupa string tunggal ditolak upstream dengan HTTP 400 (`Codex upstream HTTP 400`). Codex CLI selalu mengirim array sehingga jalur utama aman, tetapi klien lain harus memakai bentuk array.

## Monitoring refresh token

Setiap peristiwa refresh dicatat ke stdout (`pm2 logs codex-bridge`) dengan prefix `[refresh]`. **Nilai token tidak pernah dicetak** — hanya fingerprint 8 hex (SHA-256) sehingga rotasi tetap terlihat tanpa membocorkan kredensial.

Baris yang muncul, urut kejadian:

- boot: `auth source: <path>` lalu `[refresh] state fp=… exp_in=… expires_at=… last_refresh=…`
- scheduler: `[refresh] scheduler started interval=1m0s` (sekali), plus heartbeat state tiap jam
- sebelum refresh: `[refresh] start reason=… force=… old_fp=… exp_in=… expires_at=…`
- sukses: `[refresh] ok reason=… http=200 dur=… rotated=true|false new_fp=… exp_in=… expires_at=… last_refresh=…`
- gagal: `[refresh] failed reason=… http=… dur=… old_fp=… (credentials unchanged)` atau `… (upstream rotated but persist failed; re-authenticate)`
- antrean lock: `[refresh] waiting for auth.lock: another process holds it`
- dipaksa 401: `[refresh] upstream returned 401 on attempt N; forcing token refresh`

Isi `reason` menunjukkan pemicunya: `scheduler` = pengecekan berkala tiap `refresh_check_seconds`, `request` = refresh terjadi saat token mendekati kedaluwarsa pada saat dipakai request, `upstream_401` = dipaksa karena upstream menolak token. `rotated=true` berarti refresh token ikut diganti (one-time-use), jadi menjalankan lebih dari satu proses atas salinan file yang sama tetap berbahaya.

Contoh memantau:

```bash
pm2 logs codex-bridge --lines 200 | grep '\[refresh\]'
pm2 logs codex-bridge --nostream | grep -E '\[refresh\] (failed|scheduler check failed|waiting)'
```

## Rotasi token

Goroutine memeriksa kedaluwarsa JWT tiap `refresh_check_seconds` (default 60); request juga memeriksa sebelum diproses. Refresh dilakukan hanya saat token mendekati masa berlaku (dua menit), atau setelah respons 401 untuk endpoint Responses/chat. Refresh memakai `auth.lock` dan `flock` yang sama dengan Hermes pada Linux, membaca ulang file di dalam lock, lalu mengganti `auth.json` secara atomik (mode `0600`) serta menyinkronkan entri pool alias. Jangan salin refresh token ke file lain dan menjalankan dua proses yang me-refresh salinan berbeda: refresh token bersifat single-use. Simpan backup file autentikasi secara aman; jika refresh gagal karena invalid_grant, login ulang melalui Hermes.

Gateway hanya listen pada loopback HTTP. Untuk akses dari luar host gunakan reverse proxy TLS yang dikonfigurasi terpisah. File `config.yaml` berisi API key dan harus tetap `0600`; jangan commit ke Git.

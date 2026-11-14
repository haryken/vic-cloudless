# Xiaozhi TTS quá lớn → backlog tính năng âm lượng (2026-11-14)

## Hiện tượng

TTS Plan B (ALSA / MultiMedia2) nghe **to hơn hẳn** so với cảm giác “code gốc” (ExternalAudio / Wwise).  
**Không** phải do commit sửa “vấp chữ”.

## Root cause thật (so với code gốc)

| | Code gốc (ExternalAudio) | Hiện tại (Plan B ALSA) |
|--|--------------------------|-------------------------|
| Path | PCM → vic-anim SdkAudio / **Wwise** | PCM → `xiaozhi-play` → **hw:0,1 MM2** |
| `master_volume` robot | Wwise attenuate theo level | **Bỏ qua hoàn toàn** |
| Robot này | `master_volume: **0**` = `VOLUME_LOW` | Vẫn LOW trên giấy, nhưng ALSA không đọc |
| PCM ra loa | Bị Wwise kéo nhỏ theo LOW | `peak_out ≈ **-1 dBFS**` (gần max) × RX 84 |

→ Cùng `tts_gain_pct=100` và cùng file config, Plan B vẫn **to hơn nhiều** vì mất tầng volume Wwise.  
Đó là lý do “gốc không lớn vậy”.

**Thông số bám cho tính năng chỉnh âm lượng:** map `master_volume` (0–4) **hoặc** slider `tts_gain_pct` xuống soft attenuate trên path ALSA (cho phép &lt; 100%). Đừng chỉ nhìn RX=84.

## Đối chiếu commit (đừng lẫn với volume)

Branch: `xiaozhi-wireos-save-2026-07-14`

| Thứ | Commit / state | Việc làm | Ảnh hưởng volume? |
|-----|----------------|----------|-------------------|
| Sửa vấp | `8e2e836` (+ `fa52d58`) | `xiaozhi-play` **period 1024×8 (512ms)**; `minGapBeforePad = **460ms**` — không drip silence vào gap ~400ms (đó là “vấp”) | Không |
| Thử sai (uncommitted / deploy tay) | session 2026-11-14 | Đẩy thử `period_count=16` + pad **900ms** | Gây **mất tiếng** (`pcm_open EINVAL`), không làm to |
| Cái to | tinymix tay khi debug mute | `RX1/RX2/RX3 Digital Volume = **100**` (+ Spearker Boost lúc test) | **Có — nguyên nhân chính** |

Code đúng theo commit vấp (HEAD):

```text
extra/xiaozhi-play.c          period_count = 8
internal/xiaozhi/alsa_player.go  minGapBeforePad = 460ms
                              log: silence after 460ms
```

## Vì sao to (root cause cho feature volume)

### 1. Codec Digital Volume bị kéo lên 100 khi debug mute

Lúc `xiaozhi-play`×16 fail (im tiếng), debug đã:

```bash
tinymix set "RX1 Digital Volume" 100
tinymix set "RX2 Digital Volume" 100
tinymix set "RX3 Digital Volume" 100
# có lúc: tinymix set "Speaker Boost" 1
```

Baseline trước đó ~**RX*=84** (range 0→124). 100 ≈ gần trần → rất to.

Đã hạ lại 84 / Spearker Boost off trên robot sau khi user báo.

### 2. Plan B ALSA không đi volume Wwise / master Vector

```text
Opus decode → applyTTSGain → FIFO → xiaozhi-play → hw:0,1 (MM2)
```

Núm volume app / `master_volume` Vector **không** attenuate đầy đủ path này.  
Muốn chỉnh to/nhỏ ổn định phải có **control riêng cho Xiaozhi ALSA** (hoặc map từ master volume sang Digital Volume / soft gain).

### 3. PCM server đã hot ở `tts_gain=100%`

```text
tts_gain=100%
server audio peaks ~0.5–2 dB over full scale → limiter → peak_out ≈ -1 dBFS
```

`tts_gain_pct` floor hiện tại = 100 (`tts_gain.go`: min 100, “100% = source as-is”).  
Không thể hạ nhỏ hơn source bằng config hiện có — chỉ boost lên tới 1000%.

## Thông số bám khi làm slider âm lượng (đo 2026-11-14 ~09:38–09:39)

Snapshot robot lúc user bảo “vẫn to lắm”:

| Thông số | Giá trị | Ý nghĩa |
|----------|---------|---------|
| `tts_gain_pct` (file) | **100** trong `/data/.../xiaozhi/xiaozhi.json` | UI/config hiện tại |
| `tts_gain` (log turn) | có turn **100%**, có turn **1000%** | 1000% = trần boost → rất to; bám log `tts_gain=%d%%` |
| `peak_decode` | **~1.6–2.0 dBFS** | Opus server đã vượt full scale |
| `peak_out` | **-1.0 dBFS** | Sau limiter, PCM ra loa vẫn gần max |
| `overFS_frames` | hàng chục / turn | Nhiều frame phải limit |
| `RX1/RX2/RX3 Digital Volume` | **84** / 124 | Codec bình thường — **không** phải nguyên nhân “gào” lúc này |
| `Speaker Boost` | DISABLE | OK |

**Control đã mở (2026-11-15):** `tts_gain_pct` **25–1000**

| Chip / giá trị | Tai nghe |
|----------------|----------|
| **30%** (tag nhỏ) | ≈ cũ ExternalAudio + `VOLUME_LOW` |
| **100%** (tag to) | ALSA full như hiện tại (`peak_out ≈ −1 dBFS`) |
| &gt;100% | boost voice band (như trước) |

Unset / 0 trong JSON vẫn → **100** (to). Đừng bám RX làm slider chính.

## Backlog: tính năng âm lượng Xiaozhi

Khi làm feature, tách rõ 3 lớp:

| Lớp | Control | Ghi chú |
|-----|---------|---------|
| A. Soft gain | Cho phép `tts_gain_pct` **&lt; 100** (vd 40–100) trên path ALSA | Nhanh, không đụng codec; khớp UI % — **nên là default control** |
| B. Codec | Set `RX1/RX2/RX3 Digital Volume` (và không Spearker Boost mặc định) | Persist sau reboot; đừng để debug tinymix “dính” |
| C. UX | Slider “AI voice” trong wired / MCP `self.audio_speaker.set_volume` map đúng sang A hoặc B trên Plan B | Hiện MCP volume chủ yếu intent Wwise — Plan B cần wire lại |

Checklist implement:

1. Đọc volume lúc start TTS / `alsaStart` (file config hoặc jdoc).
2. Apply soft attenuate **trước** FIFO (có probe `peak_out`).
3. Optional: sync tinymix RX* một lần, không hardcode 100.
4. Test: cùng câu TTS ở 50% / 84 codec vs 100 codec; confirm master Vector không còn “ảo” trên MM2.
5. Không đổi `minGapBeforePad` / `period_count` khi làm volume — đó là track **vấp / underrun**.

## Liên quan nhanh

- Mất tiếng cùng ngày: deploy nhầm `period_count=16` → `pcm_open: Invalid argument`; FIFO O_RDWR vẫn “playing” → drain timeout `pending≈…B`.
- Vấp chữ: gap pad sớm hơn ring (vd pad 400ms khi gap server ~400–500ms) — giữ **460ms + ×8** theo commit.

package music

/*
// github.com/tosone/minimp3 (Google TTS) links the same implementation with the
// stock symbol names — rename ours so the two copies cannot collide.
#define mp3dec_init         wireos_music_mp3dec_init
#define mp3dec_decode_frame wireos_music_mp3dec_decode_frame
#define mp3dec_f32_to_s16   wireos_music_mp3dec_f32_to_s16
#define MINIMP3_IMPLEMENTATION
#include "minimp3.h"
*/
import "C"

import (
	"errors"
	"io"
	"unsafe"
)

const (
	mp3ReadChunk = 16 * 1024
	// minimp3 wants ~16KB in view: it locks onto a stream only after several
	// consecutive frame headers match (≈10KB at 320kbps).
	mp3MinBuffered = 16 * 1024
	mp3MaxBuffered = 96 * 1024
)

var errNotMP3 = errors.New("stream is not MP3")

// mp3Decoder turns an MP3 byte stream into mono s16le PCM, one frame per call.
type mp3Decoder struct {
	r      io.Reader
	dec    C.mp3dec_t
	info   C.mp3dec_frame_info_t
	pcm    [C.MINIMP3_MAX_SAMPLES_PER_FRAME]C.mp3d_sample_t
	buf    []byte
	eof    bool
	frames int

	// Rate and Channels describe the source; output is always mono at Rate.
	Rate     int
	Channels int
}

func newMP3Decoder(r io.Reader) *mp3Decoder {
	d := &mp3Decoder{r: r, buf: make([]byte, 0, mp3MaxBuffered)}
	C.mp3dec_init(&d.dec)
	return d
}

func (d *mp3Decoder) fill() error {
	if d.eof {
		return nil
	}
	if cap(d.buf)-len(d.buf) < mp3ReadChunk {
		d.buf = append(make([]byte, 0, mp3MaxBuffered), d.buf...)
	}
	n, err := d.r.Read(d.buf[len(d.buf):cap(d.buf)])
	d.buf = d.buf[:len(d.buf)+n]
	if err == io.EOF {
		d.eof = true
		return nil
	}
	return err
}

// Next returns the next decoded frame as mono s16le PCM, or io.EOF at the end.
func (d *mp3Decoder) Next() ([]byte, error) {
	skipped := 0
	for {
		for !d.eof && len(d.buf) < mp3MinBuffered {
			if err := d.fill(); err != nil {
				return nil, err
			}
		}
		if len(d.buf) == 0 {
			return nil, io.EOF
		}
		samples := C.mp3dec_decode_frame(&d.dec,
			(*C.uint8_t)(unsafe.Pointer(&d.buf[0])), C.int(len(d.buf)),
			&d.pcm[0], &d.info)
		used := int(d.info.frame_bytes)
		if used == 0 {
			if d.eof {
				return nil, io.EOF
			}
			if len(d.buf) <= mp3MaxBuffered-mp3ReadChunk {
				// Possibly a frame split at the buffer end — read more first.
				if err := d.fill(); err != nil {
					return nil, err
				}
				continue
			}
			skipped += len(d.buf) / 2
			if d.frames == 0 && skipped > 256*1024 {
				return nil, errNotMP3
			}
			d.buf = d.buf[len(d.buf)/2:]
			continue
		}
		d.buf = d.buf[used:]
		if samples == 0 {
			// ID3 tag / junk between frames.
			skipped += used
			if d.frames == 0 && skipped > 256*1024 {
				return nil, errNotMP3
			}
			continue
		}
		d.frames++
		d.Rate = int(d.info.hz)
		d.Channels = int(d.info.channels)
		pcm := unsafe.Slice((*int16)(unsafe.Pointer(&d.pcm[0])), int(samples)*d.Channels)
		return downmixS16(pcm, d.Channels), nil
	}
}

// downmixS16 averages interleaved channels into mono s16le bytes.
func downmixS16(in []int16, channels int) []byte {
	if channels < 1 {
		channels = 1
	}
	n := len(in) / channels
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		sum := 0
		for c := 0; c < channels; c++ {
			sum += int(in[i*channels+c])
		}
		v := int16(sum / channels)
		out[2*i] = byte(v)
		out[2*i+1] = byte(uint16(v) >> 8)
	}
	return out
}

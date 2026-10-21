package app

import (
	"bytes"
	"math"
	"testing"
)

func TestAudioClipBoundsRejectsMillisecondOverflow(t *testing.T) {
	src := audioSource{state: AudioAvailable, size: math.MaxInt64}
	start, end := int64(math.MaxInt64/pcmBytesPerMS+1), int64(math.MaxInt64/pcmBytesPerMS+2)
	if _, _, ok := src.clipBounds(&start, &end); ok {
		t.Fatal("overflowing millisecond offsets accepted")
	}
}

func TestWriteWAVHeaderRejectsRIFFOverflow(t *testing.T) {
	var b bytes.Buffer
	if err := WriteWAVHeader(&b, int64(math.MaxUint32)-35); err == nil {
		t.Fatal("oversized RIFF payload accepted")
	}
	if err := WriteWAVHeader(&b, -1); err == nil {
		t.Fatal("negative payload accepted")
	}
}

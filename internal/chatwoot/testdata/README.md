# Synthetic call audio

`silence.mp3` contains 0.1 seconds of generated stereo silence at 8 kHz.
It contains no recorded speech, contacts, tokens, or source-account data.
Generate it with:

```sh
ffmpeg -nostdin -hide_banner -loglevel error -threads 1 \
  -f lavfi -i 'anullsrc=r=8000:cl=stereo' -t 0.1 \
  -c:a libmp3lame -b:a 24k -map_metadata -1 silence.mp3
```

`silence-no-id3.mp3` uses the same command with `-id3v2_version 0` and
`-write_id3v1 0` before the output filename to exercise an untagged MP3.

The WAV fixture is built from PCM headers and zero samples in the Go tests.

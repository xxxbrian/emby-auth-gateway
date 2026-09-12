# Indexed subtitle fixture

`indexed-subtitles.mkv` is a 783-byte local FFmpeg-generated Matroska file.
It contains no video, audio, credentials, or content from an upstream account.

- Stream index 0: SubRip, `eng`, "English first cue" / "English later cue".
- Stream index 1: SubRip, `chi`, "第一句中文字幕" / "后面的中文字幕".
- Both tracks have cues at 1.000–2.500 seconds and 20.000–21.000 seconds.

The fixture is shared by indexed-extraction and subtitle-manager tests. It was
muxed from two tiny UTF-8 SRT inputs with `-map 0:0 -map 1:0 -c:s srt`, explicit
`language=eng` / `language=chi` stream metadata, and `-fflags +bitexact`.
The extractor also generates a separate Matroska file during its optional
FFmpeg round-trip test, so parser verification is not limited to handcrafted EBML.

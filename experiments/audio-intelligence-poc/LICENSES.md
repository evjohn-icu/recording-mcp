# Model and data license audit

This audit covers the artifacts actually used by this PoC. It is not legal
advice. The key risk is the upstream CED code license (GPL-3.0), which is why
the final recommendation uses the Apache-2.0 sherpa-onnx Zipformer tagger as
the primary ambient model.

| Artifact | Role | Code license | Weight / data license | Evidence |
|---|---|---|---|---|
| Silero VAD (`silero-vad` package / `silero_vad.onnx`) | VAD | MIT | MIT | package metadata; `onnx-community/silero-vad` model card |
| sherpa-onnx | runtime | Apache-2.0 | n/a | project license |
| sherpa-onnx Zipformer audio tagging (`k2-fsa/sherpa-onnx-zipformer-audio-tagging-2024-04-09`) | ambient | Apache-2.0 | Apache-2.0 (model card) | HF model card `license: apache-2.0` |
| sherpa-onnx CED audio tagging (`k2-fsa/sherpa-onnx-ced-base-audio-tagging-2024-04-19`) | ambient | Apache-2.0 wrapper | **upstream CED is GPL-3.0**; model repo has no LICENSE | `RicherMans/CED` LICENSE = GNU GPL v3 |
| PANNs CNN14 (`Cnn14_mAP=0.431.pth`) | ambient comparison | MIT (`audioset_tagging_cnn`) | CC-BY-4.0 (Zenodo record 3987831) | Zenodo API `metadata.license.id=cc-by-4.0`; HF mirror `ayushrsys/panns-cnn14`, md5 `541141fa2ee191a88f24a3219fff024e` matches Zenodo |
| pyannote.audio | diarization runtime | MIT | n/a | project license |
| pyannote Community-1 (`pyannote/speaker-diarization-community-1`) | diarization | MIT (pyannote.audio) | CC-BY-4.0 | official gated repo (`gated: auto`), terms accepted, downloaded at revision `3533c8cf8e36`; one session (L) scored with the official weights (DER 0.0995, identical to the mirror). The 20-session aggregate still uses the public mirror `pyannote-community/speaker-diarization-community-1` |
| pyannote segmentation 3.0 (ONNX conversion) | diarization | MIT | MIT | `csukuangfj/sherpa-onnx-pyannote-segmentation-3-0/LICENSE` |
| 3D-Speaker CAM++ (`campplus_zh_en_fp32.onnx`) | voiceprint | Apache-2.0 | Apache-2.0 | third-party ONNX export `Luigi/campplus-zh-en-onnx`; upstream ModelScope 3D-Speaker. Pin/verify an official export for production. |
| WeSpeaker CN-Celeb ResNet34-LM (`cnceleb_resnet34_LM.onnx`) | voiceprint comparison | Apache-2.0 | Apache-2.0 | `Wespeaker/wespeaker-cnceleb-resnet34-LM` model card |
| AISHELL-4 test split | Chinese multi-speaker audio | n/a | **CC BY-SA 4.0** (official OpenSLR SLR111); HF dataset card is mislabeled `apache-2.0` | OpenSLR SLR111 page; verify before redistribution |
| ESC-50 | labelled ambient evaluation | n/a | **CC BY-NC 3.0 (non-commercial)**; individual clips vary | `ESC-50/LICENSE` |
| Debian bookworm-slim | container base | DFSG-free | n/a | Debian |
| CPython 3.12 portable build | container interpreter | PSF-2.0 | n/a | python.org / uv standalone build |

## Notes

- **CED / GPL-3.0**: The k2-fsa CED model repository does not ship a separate
  license file. Its model card says the model was converted from
  `RicherMans/CED`, whose repository is GPL-3.0. Treat the CED weights as
  GPL-3.0 until the model publisher states otherwise. CED is included only as
  an accuracy baseline.
- **pyannote Community-1**: The official HF repository is gated. The terms were
  accepted and the official artifact (revision `3533c8cf8e36`) was downloaded
  with a user token for one session (`L_R004S03C01`), where it reproduced the
  mirror's DER exactly. The 20-session aggregate was still produced with the
  public mirror, so a production rollout that needs every session to come
  from the official artifact must re-run those sessions and pin the revision.
- **ESC-50**: used only to measure ambient tagging; it is not redistributed
  and does not grant commercial rights. A production evaluation should use a
  commercially cleared ambient corpus.
- **AISHELL-4**: official OpenSLR SLR111 license is **CC BY-SA 4.0** (attribution + share-alike). The HF mirror card says `apache-2.0`, but the authoritative OpenSLR page is CC BY-SA 4.0. The PoC uses the test split and channel 0 only and does not redistribute the audio.
- **Model weights trained on AudioSet**: AudioSet annotations are CC-BY-4.0;
  the underlying YouTube audio is not redistributed here.

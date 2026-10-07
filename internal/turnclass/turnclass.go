// Package turnclass classifies transcript turns as spoken conversation or
// ambient sound (voice-assistant commands, transit broadcasts, kiosk prompts,
// media playback) so the semantic layer can derive from conversation only and
// still keep ambient content searchable and auditable.
//
// Classification is content-derived and deterministic: the same text always
// yields the same class, so rows keyed by turn id survive transcript
// re-publication as long as the turn content is unchanged.
package turnclass

import "regexp"

// Class values stored in turn_classes.class.
const (
	ClassSpeech  = "speech"
	ClassAmbient = "ambient"
)

// Subtype values stored in turn_classes.subtype for ambient turns; empty for
// speech turns. Rules only cover the patterns that are unambiguous from text
// alone — lyrics and fiction narration are left to the model pass.
const (
	SubtypeVoiceAssistant   = "voice_assistant"
	SubtypeTransitBroadcast = "transit_broadcast"
	SubtypeKioskPrompt      = "kiosk_prompt"
	SubtypeMediaPlayback    = "media_playback"
	// SubtypeMusicLyrics is model-pass only: lyrics are not recognizable
	// from a deterministic text rule.
	SubtypeMusicLyrics = "music_lyrics"
)

// Method values stored in turn_classes.method.
const (
	MethodRule    = "rule"
	MethodModel   = "model"
	MethodDerived = "derived"
)

var rules = []struct {
	subtype string
	re      *regexp.Regexp
}{
	{SubtypeVoiceAssistant, regexp.MustCompile(`^(小爱同学|小度|小艺|天猫精灵|嘿siri|hi\s*siri)[，,。．.！!\s]*`)},
	{SubtypeVoiceAssistant, regexp.MustCompile(`^(帮我)?(打开|关闭|开启|关掉|调高|调低|调大|调小|播放|暂停)(一下)?(空调|电视|音箱|灯光|灯|窗帘|风扇|加湿器|音乐)[。！!．.]*$`)},
	{SubtypeTransitBroadcast, regexp.MustCompile(`(列车前方到站|前方到站|本次列车|下一站|各位乘客|乘客们请注意|请从[^。]{0,8}(门|口)下车|请(携带|带好)随身物品|到站的乘客|可换乘[^。]{0,12}(地铁|有轨电车|公交|号线)|地铁[^。]{0,6}报站)`)},
	{SubtypeKioskPrompt, regexp.MustCompile(`(扫码点餐|请扫码|扫码[^。]{0,6}(支付|关注|领取|点餐)|请凭[^。]{0,10}(小票|号码|取餐码)取|您的(订单|餐品|取餐号)|点餐(成功|完成)|自助(点餐|取餐)|欢迎光临|请到[^。]{0,8}取餐)`)},
	{SubtypeMediaPlayback, regexp.MustCompile(`(上集说到|未完待续|点击关注|点赞(订阅|关注)|本(集|期)(节目|视频)|直播间[^。]{0,6}(人数|观众)|正在播放[^。]{0,10}(歌|音乐|专辑))`)},
}

// Classify inspects one turn's text with the deterministic ambient rules. It
// returns ClassAmbient and the matched subtype when a rule fires, otherwise
// ClassSpeech with an empty subtype. matched reports whether a rule fired at
// all, so callers can tell "rule says speech" from "rules never saw it" and
// route the latter to the model pass.
func Classify(content string) (class, subtype string, matched bool) {
	for _, r := range rules {
		if r.re.MatchString(content) {
			return ClassAmbient, r.subtype, true
		}
	}
	return ClassSpeech, "", false
}

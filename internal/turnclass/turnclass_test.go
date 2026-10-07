package turnclass

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		content string
		class   string
		subtype string
		matched bool
	}{
		// Real samples from the recording corpus.
		{"唤醒词单独成轮", "小爱同学。", ClassAmbient, SubtypeVoiceAssistant, true},
		{"设备指令单独成轮", "打开空调。", ClassAmbient, SubtypeVoiceAssistant, true},
		{"播报换乘", "可换乘南海有轨电车1号线。", ClassAmbient, SubtypeTransitBroadcast, true},
		{"设备清单是人说的话", "啊，LED屏，啊，音箱，啊，对讲广播这些。", ClassSpeech, "", false},
		{"谈论地铁是人的话", "我坐地铁过来的，大概半小时。", ClassSpeech, "", false},
		{"谈论空调是人的话", "会议室的空调坏了要修一下。", ClassSpeech, "", false},
		{"报站广播", "列车前方到站，祖庙站，请从左侧车门下车。", ClassAmbient, SubtypeTransitBroadcast, true},
		{"点餐提示", "扫码点餐成功，请凭取餐号到取餐口取餐。", ClassAmbient, SubtypeKioskPrompt, true},
		{"媒体片头", "上集说到主角进入集市，未完待续。", ClassAmbient, SubtypeMediaPlayback, true},
		{"普通业务句", "Hera产品的按钮手感偏软，有浮动感。", ClassSpeech, "", false},
		{"歌词规则不猜", "不断追逐不断飞，疲惫但不想停下。", ClassSpeech, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, subtype, matched := Classify(tc.content)
			if class != tc.class || subtype != tc.subtype || matched != tc.matched {
				t.Errorf("Classify(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.content, class, subtype, matched, tc.class, tc.subtype, tc.matched)
			}
		})
	}
}

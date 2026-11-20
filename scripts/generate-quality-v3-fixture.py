#!/usr/bin/env python3
"""Deterministic generator for the frozen retrieval-quality-v3 fixture (r1).

Writes corpus.json / retrieval.json / lifecycle.json plus SHA256SUMS.json,
AUTHORING.md and REVIEW.md into the target fixture directory. Every run is
byte-identical: no timestamps, no randomness, all content is index-derived.

`--self-check` validates every structural rule in memory (counts, group
composition, split isolation, query uniqueness, lifecycle ownership) and
exits 0 without writing anything.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
from dataclasses import dataclass
from datetime import date, timedelta
from pathlib import Path

SPLITS = ("train", "calibration", "test")
BUCKETS = (
    "direct_fact", "synonym", "negation", "time_change", "cross_session",
    "multi_source", "revocation", "missing_evidence", "wrong_identity_proposal",
    "numeric_constraint", "decision_commitment",
)
PARTIAL_BUCKETS = ("time_change", "revocation", "missing_evidence", "wrong_identity_proposal")
TOP1_BUCKETS = tuple(b for b in BUCKETS if b not in ("cross_session", "multi_source"))
BUCKET_PREFIX = {
    "direct_fact": "df", "synonym": "syn", "negation": "neg", "time_change": "tc",
    "cross_session": "xs", "multi_source": "ms", "revocation": "rev",
    "missing_evidence": "me", "wrong_identity_proposal": "wip",
    "numeric_constraint": "num", "decision_commitment": "dc",
}
GROUPS_PER_BUCKET_SPLIT = 10
REVISION_ORDER = ("r1", "r2")
TURN_MS = 8000
SEGMENT_DURATION_MS = 60000
BASE_DATE = date(2026, 3, 2)
CJK = re.compile(r"[\u3400-\u9fff]")
CODE_RE = re.compile(r"\d{3,}")
SESSION_SUFFIX_RE = re.compile(r"(.+?)-session-[12]")

VENUES = (
    "东湖文化站", "南岭社区中心", "白桦书院", "青溪便民服务中心", "石桥邻里中心",
    "望山文体馆", "柳荫社区学院", "银杏湾活动中心", "清源便民服务站", "翠微邻里汇",
    "长风文体中心", "栖霞社区驿站", "鹿鸣综合服务馆", "汇泉邻里之家", "云台文体驿站",
    "星桥社区中心", "朗月便民活动站", "松涛邻里文化站", "荷风社区会堂", "白鹭湾服务中心",
    "听雨轩社区服务站", "琥珀山邻里中心", "望江文体汇", "梅岭社区之家", "青竹便民活动馆",
    "临溪文体驿站", "曙光社区中心", "环碧邻里汇", "叠翠文化站", "金滩社区服务馆",
    "虹湾活动中心", "枫林便民驿站", "澜山社区学院", "汀兰邻里中心", "远航文体馆",
    "晨光社区服务站",
)
PERSONS = (
    "沈明远", "顾清岚", "陆知行", "白若曦", "江晓帆", "程一鸣",
    "秦望舒", "孟繁星", "阮静秋", "傅博文", "祁雨桐", "文彦清",
    "韩霁月", "岑砚秋", "姚青禾", "聂海平", "温书言", "邹岚欣",
    "龙沛然", "蓝溪亭", "简云舒", "车明轩", "苏皖晴", "陶辞秋",
)
MONTHS = ("三月", "四月", "五月", "六月", "七月", "八月", "九月", "十月")

BUCKET_NOTES = {
    "direct_fact": "同义直问：查询直接指向事实话轮",
    "synonym": "同义改写：查询换一种说法指向同一事实",
    "negation": "否定排除：事实话轮包含明确的否定或排除表述",
    "time_change": "时间变化：r1 旧值与 r2 当前值冲突，complete 金标只指向当前话轮",
    "cross_session": "跨会话：两个会话各提供一半事实，complete 金标引用两个 conversation",
    "multi_source": "同会话多来源：complete 金标引用同一会话的两个话轮",
    "revocation": "撤回：r1 旧通知被 r2 撤回公告取代；旧话轮列入 lifecycle.json",
    "missing_evidence": "证据缺口：partial 问题只有一半材料，按协议标记 insufficient",
    "wrong_identity_proposal": "身份提议：未确认 speaker 提出人员提议，complete 金标不断言身份已确认",
    "numeric_constraint": "数值约束：查询中的数字出现在其话轮文本中",
    "decision_commitment": "决定承诺：事实话轮是明确的最终决定表述",
}


@dataclass(frozen=True)
class GroupContext:
    bucket: str
    tag: str
    split: str
    k: int
    venue: str
    code: int
    person: str
    month: str


def _started_at(day_offset: int) -> str:
    d = BASE_DATE + timedelta(days=day_offset)
    return f"{d.year:04d}-{d.month:02d}-{d.day:02d}T08:00:00+08:00"


def render_turn(spec: dict, index: int) -> dict:
    start = (index - 1) * TURN_MS
    return {
        "id": spec["id"],
        "original_index": index,
        "text": spec["text"],
        "global_start_ms": start,
        "global_end_ms": start + TURN_MS,
        "local_start_ms": start,
        "local_end_ms": start + TURN_MS,
        "speaker_scoped_id": spec.get("speaker", "speaker:host"),
        "speaker_entity_id": None,
        "confidence": 0.98,
    }


def _question(qid: str, group: str, split: str, bucket: str, query: str, refs: list,
              unanswerable: bool, expected: str, completeness: str, notes: str,
              scenario_kind: str, gold_answer, top1=None) -> dict:
    q = {
        "id": qid,
        "group_id": group,
        "source_group_id": group,
        "split": split,
        "bucket": bucket,
        "query": query,
        "purpose": "no_evidence" if unanswerable else bucket,
        "gold_turn_refs": refs,
        "gold_evidence_turn_ids": [],
        "gold_topic_refs": [],
        "gold_topic_ids": [],
        "unanswerable": unanswerable,
    }
    if top1 is not None:
        q["gold_top1_turn_ref"] = top1
    q.update({
        "scope": {},
        "expect_ambient_excluded": True,
        "expect_stale_excluded": True,
        "notes": notes,
        "expected_sufficiency": expected,
        "evidence_completeness": completeness,
        "scenario_kind": scenario_kind,
        "gold_answer": gold_answer,
    })
    return q


# ---------------------------------------------------------------------------
# Per-bucket scenario builders. Every query embeds the group's unique numeric
# code literal (`编号{code}`), and every gold-cited turn embeds the same code,
# so leakage and mix-ups are observable.
# ---------------------------------------------------------------------------

def build_direct_fact(g: GroupContext) -> dict:
    topic = f"{g.venue}储物柜钥匙存放"
    return {
        "topic": topic,
        "convs": [{"revisions": {"r1": [
            {"id": "fact", "text": f"{g.venue}编号{g.code}的储物柜钥匙统一存放在一楼服务台的带锁抽屉里。"},
            {"id": "filler", "text": f"{g.venue}的储物柜使用登记表由值班员每周五汇总归档。"},
        ]}}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的储物柜钥匙放在哪里？",
            "refs": [(0, "r1", "fact")], "top1": (0, "r1", "fact"),
            "answer": "钥匙统一存放在一楼服务台的带锁抽屉里。",
            "notes": f"{topic}。黄金答案：钥匙在一楼服务台的带锁抽屉。证据说明：事实话轮直接给出编号{g.code}储物柜钥匙的存放位置；另一话轮是登记归档流程，不是答案。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的储物柜租金是多少钱一个月？",
            "notes": f"{topic}。无证据说明：记录只有钥匙存放位置和登记流程，没有租金信息。",
        },
        "partial": None,
    }


def build_synonym(g: GroupContext) -> dict:
    topic = f"{g.venue}借阅证借阅上限"
    return {
        "topic": topic,
        "convs": [{"revisions": {"r1": [
            {"id": "fact", "text": f"{g.venue}的编号{g.code}借阅证每次最多可以外借八册图书，借期三十天。"},
            {"id": "filler", "text": f"{g.venue}的阅览座位无需证件，登记后即可使用。"},
        ]}}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的读者证一次能借几本书？",
            "refs": [(0, "r1", "fact")], "top1": (0, "r1", "fact"),
            "answer": "每次最多外借八册图书。",
            "notes": f"{topic}。黄金答案：每次最多外借八册。证据说明：查询用“读者证/借几本书”同义改写“借阅证/外借几册”，事实话轮直接回答；座位话轮是干扰项。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的借阅证工本费是多少？",
            "notes": f"{topic}。无证据说明：记录只有借阅上限与借期，没有工本费。",
        },
        "partial": None,
    }


def build_negation(g: GroupContext) -> dict:
    topic = f"{g.venue}活动室开放安排"
    return {
        "topic": topic,
        "convs": [{"revisions": {"r1": [
            {"id": "fact", "text": f"{g.venue}编号{g.code}的活动室工作日不对外开放，只有周末可以预约使用。"},
            {"id": "filler", "text": f"{g.venue}活动室的音响设备由管理员统一开关。"},
        ]}}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的活动室周一可以预约吗？",
            "refs": [(0, "r1", "fact")], "top1": (0, "r1", "fact"),
            "answer": "工作日不对外开放，只有周末可以预约；周一不能预约。",
            "notes": f"{topic}。黄金答案：周一不能预约。证据说明：事实话轮包含明确否定（工作日不开放），是问题的直接答案；设备话轮无关。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的活动室每小时收费多少钱？",
            "notes": f"{topic}。无证据说明：记录只有开放范围，没有收费标准。",
        },
        "partial": None,
    }


def build_time_change(g: GroupContext) -> dict:
    topic = f"{g.venue}展厅开放时间调整"
    return {
        "topic": topic,
        "convs": [{"revisions": {
            "r1": [{"id": "old", "text": f"{g.venue}编号{g.code}展厅此前公告的开放时间为上午九点开始。"}],
            "r2": [{"id": "current", "text": f"{g.venue}编号{g.code}展厅自{g.month}起开放时间调整为上午十点，旧时间公告同时废止。"}],
        }}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的展厅现在几点开门？",
            "refs": [(0, "r2", "current")], "top1": (0, "r2", "current"),
            "answer": "现在上午十点开门。",
            "notes": f"{topic}。黄金答案：上午十点。证据说明：r1 旧公告与 r2 现行公告的开放时间冲突，问题问当前值，因此只标 r2 当前话轮。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的展厅门票每张多少钱？",
            "notes": f"{topic}。无证据说明：新旧记录只有开放时间，没有票价信息。",
        },
        "partial": {
            "query": f"{g.venue}编号{g.code}展厅这次开门时间调整的具体实施日期是哪一天？",
            "refs": [(0, "r2", "current")],
            "notes": f"{topic}。部分证据说明：现行公告只写明自{g.month}起调整，没有具体实施日期；旧公告仅记录旧时间。单条证据不足以完整回答，按协议标记 expected_sufficiency=insufficient。",
        },
    }


def build_cross_session(g: GroupContext) -> dict:
    topic = f"{g.venue}亲子手工课安排"
    return {
        "topic": topic,
        "convs": [
            {"revisions": {"r1": [{"id": "fact1", "text": f"{g.venue}编号{g.code}的亲子手工课集合地点在东门客服中心。"}]}},
            {"revisions": {"r1": [{"id": "fact2", "text": f"{g.venue}编号{g.code}的亲子手工课上午九点半准时出发，请提前十分钟到场。"}]}},
        ],
        "complete": {
            "query": f"{g.venue}编号{g.code}的亲子手工课在哪里集合，几点出发？",
            "refs": [(0, "r1", "fact1"), (1, "r1", "fact2")], "top1": None,
            "answer": "在东门客服中心集合，上午九点半出发。",
            "notes": f"{topic}。黄金答案必须合并两个会话：集合地点与出发时间分别来自两次记录，缺一不可。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的亲子手工课每人收费多少钱？",
            "notes": f"{topic}。无证据说明：两条会话记录只有集合地点与出发时间，没有收费信息。",
        },
        "partial": None,
    }


def build_multi_source(g: GroupContext) -> dict:
    topic = f"{g.venue}义诊活动安排"
    return {
        "topic": topic,
        "convs": [{"revisions": {"r1": [
            {"id": "fact1", "text": f"{g.venue}编号{g.code}的义诊活动在二楼多功能厅举行。"},
            {"id": "fact2", "text": f"{g.venue}编号{g.code}的义诊活动需要提前在社区服务台登记姓名和联系电话。"},
        ]}}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的义诊活动在哪里举行，参加前要做什么？",
            "refs": [(0, "r1", "fact1"), (0, "r1", "fact2")], "top1": None,
            "answer": "在二楼多功能厅举行；参加前需要提前在社区服务台登记姓名和联系电话。",
            "notes": f"{topic}。黄金答案必须合并同一会话的两个话轮：地点与登记要求缺一不可。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的义诊活动由哪位医生坐诊？",
            "notes": f"{topic}。无证据说明：两条记录只有地点与登记要求，没有医生姓名。",
        },
        "partial": None,
    }


def build_revocation(g: GroupContext) -> dict:
    topic = f"{g.venue}游园活动场地通知撤回"
    return {
        "topic": topic,
        "convs": [{"revisions": {
            "r1": [{"id": "old", "text": f"{g.venue}编号{g.code}的游园活动原定在中央草坪举行，此前通知写明了这一安排。"}],
            "r2": [{"id": "current", "text": f"{g.venue}编号{g.code}游园活动的原场地通知已正式撤回，最新安排改为风雨操场。"}],
        }}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的游园活动现在定在哪里举行？",
            "refs": [(0, "r2", "current")], "top1": (0, "r2", "current"),
            "answer": "现在定在风雨操场举行。",
            "notes": f"{topic}。黄金答案：风雨操场。证据说明：r1 旧场地通知已被 r2 撤回公告取代，问题问当前安排，因此只标 r2 话轮；旧话轮列入 lifecycle，须在遗忘后不可检索。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的游园活动雨天备用物资清单在哪里领取？",
            "notes": f"{topic}。无证据说明：新旧记录只有场地安排与撤回信息，没有物资清单。",
        },
        "partial": {
            "query": f"{g.venue}编号{g.code}游园活动最初的通知写了什么场地，这份通知后来是怎么处理的？",
            "refs": [(0, "r1", "old")],
            "notes": f"{topic}。部分证据说明：旧通知只写明原定中央草坪；撤回处理与最新场地在现行公告中，仅凭旧通知不足以完整回答，按协议标记 expected_sufficiency=insufficient。",
        },
    }


def build_missing_evidence(g: GroupContext) -> dict:
    topic = f"{g.venue}游泳培训班安排"
    return {
        "topic": topic,
        "convs": [{"revisions": {"r1": [
            {"id": "fact", "text": f"{g.venue}编号{g.code}的游泳培训班每天上午八点到九点在室内泳池上课。"},
            {"id": "extra", "text": f"{g.venue}游泳培训班报名需要携带健康承诺书到前台登记。"},
        ]}}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的游泳培训班每天几点上课？",
            "refs": [(0, "r1", "fact")], "top1": (0, "r1", "fact"),
            "answer": "每天上午八点到九点。",
            "notes": f"{topic}。黄金答案：上午八点到九点。证据说明：事实话轮直接给出上课时间；报名话轮是相邻但不同属性的信息。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的游泳培训班教练是哪一位？",
            "notes": f"{topic}。无证据说明：记录只有上课时间与报名要求，没有教练姓名。",
        },
        "partial": {
            "query": f"{g.venue}编号{g.code}的游泳培训班报名要带哪些材料，名额名单什么时候公布？",
            "refs": [(0, "r1", "extra")],
            "notes": f"{topic}。部分证据说明：报名话轮写明需携带健康承诺书；名额公布时间没有任何记录，单条证据不足以完整回答，按协议标记 expected_sufficiency=insufficient。",
        },
    }


def build_wrong_identity_proposal(g: GroupContext) -> dict:
    topic = f"{g.venue}读书会登记人员提议"
    return {
        "topic": topic,
        "convs": [{"revisions": {"r1": [
            {"id": "proposal", "speaker": "speaker:guest-1",
             "text": f"有人提议由{g.person}负责{g.venue}编号{g.code}读书会的登记工作，但该提议尚未得到确认。"},
            {"id": "filler", "text": f"{g.venue}读书会的场地申请由行政前台统一受理。"},
        ]}}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的读书会登记工作目前是谁被提议负责，确定了吗？",
            "refs": [(0, "r1", "proposal")], "top1": (0, "r1", "proposal"),
            "answer": f"目前只有提议由{g.person}负责，该提议尚未确认，没有最终任命。",
            "notes": f"{topic}。黄金答案只报告未确认提议与“尚未确认”状态，不断言身份已确认；流程话轮是干扰项。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的读书会每周固定在星期几举行？",
            "notes": f"{topic}。无证据说明：记录只有人员提议与场地流程，没有固定举行日期。",
        },
        "partial": {
            "query": f"{g.venue}编号{g.code}读书会的登记工作最终由谁负责？",
            "refs": [(0, "r1", "proposal")],
            "notes": f"{topic}。部分证据说明：只有一条未确认的人员提议，没有最终任命记录；不能把提议当作确认事实，按协议标记 expected_sufficiency=insufficient。",
        },
    }


def build_numeric_constraint(g: GroupContext) -> dict:
    topic = f"{g.venue}会议室使用申请"
    return {
        "topic": topic,
        "convs": [{"revisions": {"r1": [
            {"id": "fact", "text": f"{g.venue}编号{g.code}的会议室超过二十人时需要提前两天提交使用申请。"},
            {"id": "filler", "text": f"{g.venue}会议室的投影设备借用需要登记工牌。"},
        ]}}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的会议室超过二十人时要提前几天提交申请？",
            "refs": [(0, "r1", "fact")], "top1": (0, "r1", "fact"),
            "answer": "需要提前两天提交使用申请。",
            "notes": f"{topic}。黄金答案：提前两天。证据说明：查询中的“二十人”约束与话轮文本一致，事实话轮直接给出提前量；设备话轮无关。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}的会议室使用费每小时多少钱？",
            "notes": f"{topic}。无证据说明：记录只有申请规则，没有收费标准。",
        },
        "partial": None,
    }


def build_decision_commitment(g: GroupContext) -> dict:
    topic = f"{g.venue}场地租期决定"
    return {
        "topic": topic,
        "convs": [{"revisions": {"r1": [
            {"id": "fact", "text": f"关于{g.venue}编号{g.code}场地的租期，管理员会议已最终决定续租三年，本月底签署协议。"},
            {"id": "filler", "text": f"{g.venue}场地的消防检查安排在下月初进行。"},
        ]}}],
        "complete": {
            "query": f"{g.venue}编号{g.code}的场地租期最终决定是什么？",
            "refs": [(0, "r1", "fact")], "top1": (0, "r1", "fact"),
            "answer": "最终决定续租三年。",
            "notes": f"{topic}。黄金答案：续租三年。证据说明：事实话轮是已作出的最终决定表述；消防检查话轮无关。",
        },
        "none": {
            "query": f"{g.venue}编号{g.code}场地的年租金是多少钱？",
            "notes": f"{topic}。无证据说明：记录只有租期决定，没有租金金额。",
        },
    }


BUILDERS = {
    "direct_fact": build_direct_fact,
    "synonym": build_synonym,
    "negation": build_negation,
    "time_change": build_time_change,
    "cross_session": build_cross_session,
    "multi_source": build_multi_source,
    "revocation": build_revocation,
    "missing_evidence": build_missing_evidence,
    "wrong_identity_proposal": build_wrong_identity_proposal,
    "numeric_constraint": build_numeric_constraint,
    "decision_commitment": build_decision_commitment,
}

assert len(VENUES) == 36 and len(set(VENUES)) == 36
assert len(PERSONS) == 24 and len(set(PERSONS)) == 24
assert set(BUILDERS) == set(BUCKETS)


# ---------------------------------------------------------------------------
# Assembly
# ---------------------------------------------------------------------------

def build_fixture() -> tuple[dict, dict, dict]:
    conversations: list[dict] = []
    questions: list[dict] = []
    lifecycle_sources: list[dict] = []
    gidx = 0
    for b_idx, bucket in enumerate(BUCKETS):
        for s_idx, split in enumerate(SPLITS):
            for k in range(1, GROUPS_PER_BUCKET_SPLIT + 1):
                tag = f"{BUCKET_PREFIX[bucket]}-{split}-{k:02d}"
                g = GroupContext(
                    bucket=bucket, tag=tag, split=split, k=k,
                    venue=VENUES[(b_idx * 3 + s_idx * 10 + (k - 1)) % len(VENUES)],
                    code=101 + gidx,
                    person=PERSONS[(gidx * 7 + 3) % len(PERSONS)],
                    month=MONTHS[gidx % len(MONTHS)],
                )
                scenario = BUILDERS[bucket](g)
                conv_ids: list[str] = []
                for ci, conv_spec in enumerate(scenario["convs"]):
                    conv_id = f"c-{tag}" if len(scenario["convs"]) == 1 else f"c-{tag}-session-{ci + 1}"
                    conv_ids.append(conv_id)
                    base = conv_id[2:]
                    segments = []
                    previous_rev = None
                    for rev in REVISION_ORDER:
                        turn_specs = conv_spec["revisions"].get(rev)
                        if not turn_specs:
                            continue
                        day_offset = gidx + (30 if rev == "r2" else 0) + (1 if ci > 0 else 0)
                        source_block = {
                            "provider": "synthetic-v3",
                            "provider_request_id": f"req-{base}-{rev}",
                            "device_id": f"device-{conv_id}",
                            "recording_id": f"rec-{base}",
                            "conversation_id": conv_id,
                            "segment_id": f"seg-{base}",
                            "segment_offset_ms": 0,
                            "segment_duration_ms": SEGMENT_DURATION_MS,
                            "transcript_revision": rev,
                            "completion_status": "final",
                            "recording_started_at": _started_at(day_offset),
                            "capture_timezone": "Asia/Shanghai",
                            "source_audio_sha256": None,
                            "source_ref": None,
                            "received_at": None,
                            "model": "synthetic-curated",
                            "resource_id": None,
                            "server_log_id": None,
                            "raw_envelope": {},
                        }
                        if previous_rev is not None:
                            # Replacing an active revision requires the explicit
                            # supersede edge; without it ingest rejects r2.
                            source_block["supersedes_revision"] = previous_rev
                        previous_rev = rev
                        segments.append({
                            "contract_version": "asr-input/0.1",
                            "raw_source": {},
                            "source": source_block,
                            "turns": [render_turn(t, i + 1) for i, t in enumerate(turn_specs)],
                        })
                    conversations.append({
                        "conversation_id": conv_id,
                        "description": scenario["topic"],
                        "segments": segments,
                    })

                def resolve(ref: tuple) -> dict:
                    ci, rev, turn_id = ref
                    return {"segment_id": f"seg-{conv_ids[ci][2:]}", "transcript_revision": rev, "turn_id": turn_id}

                comp = scenario["complete"]
                group_questions = [
                    _question(f"{tag}-q1", tag, split, bucket, comp["query"],
                              [resolve(r) for r in comp["refs"]], False, "sufficient", "complete",
                              comp["notes"], bucket, comp["answer"],
                              top1=resolve(comp["top1"]) if comp.get("top1") else None),
                    _question(f"{tag}-q2", tag, split, bucket, scenario["none"]["query"], [],
                              True, "insufficient", "none", scenario["none"]["notes"],
                              "missing_attribute", None),
                ]
                if scenario.get("partial"):
                    part = scenario["partial"]
                    group_questions.append(
                        _question(f"{tag}-q3", tag, split, bucket, part["query"],
                                  [resolve(r) for r in part["refs"]], False, "insufficient",
                                  "partial", part["notes"], bucket, None))
                if bucket == "revocation":
                    for q in group_questions:
                        q["expect_forgotten_excluded"] = True
                    lifecycle_sources.append({
                        "segment_id": f"seg-{tag}",
                        "transcript_revision": "r1",
                        "turn_id": "old",
                        "reason": f"revocation group {tag}: 已撤回的旧通知必须在遗忘后不可检索",
                    })
                questions.extend(group_questions)
                gidx += 1
    corpus = {
        "description": ("Deterministic synthetic Chinese retrieval benchmark for v3 protocol "
                        "calibration r1. Template-authored, not human-adjudicated. Every source "
                        "is fictional; no personal recordings or production data."),
        "conversations": conversations,
    }
    retrieval = {
        "description": ("Frozen 780-question synthetic Chinese retrieval benchmark for v3 "
                        "calibration r1. Template-authored by scripts/generate-quality-v3-fixture.py; "
                        "split-isolated groups with complete/none/partial evidence labels."),
        "questions": questions,
    }
    lifecycle = {
        "schema": "retrieval-lifecycle-v1",
        "description": ("Deterministic forget list for v3 calibration r1: the superseded r1 turn "
                        "of every revocation group must be forgotten before retrieval evaluation."),
        "sources": lifecycle_sources,
    }
    return corpus, retrieval, lifecycle


# ---------------------------------------------------------------------------
# Self-check
# ---------------------------------------------------------------------------

def self_check(corpus: dict, retrieval: dict, lifecycle: dict) -> dict:
    problems: list[str] = []

    def check(cond, msg: str) -> None:
        if not cond:
            problems.append(msg)

    questions = retrieval["questions"]
    conversations = corpus["conversations"]

    sources: dict[tuple, str] = {}
    turn_by_key: dict[tuple, dict] = {}
    seg_split: dict[str, str] = {}
    conv_split: dict[str, str] = {}
    segments_by_id: dict[str, list] = {}
    for conv in conversations:
        cid = conv["conversation_id"]
        check(cid not in conv_split, f"duplicate conversation {cid}")
        base = cid[2:]
        m = SESSION_SUFFIX_RE.fullmatch(base)
        tag = m.group(1) if m else base
        parts = tag.split("-")
        check(len(parts) == 3 and parts[1] in SPLITS, f"conversation {cid} tag malformed")
        split = parts[1]
        conv_split[cid] = split
        for seg in conv["segments"]:
            src = seg["source"]
            sid = src["segment_id"]
            check(sid == f"seg-{base}", f"segment id mismatch inside {cid}")
            check(src["conversation_id"] == cid, f"source conversation_id mismatch in {sid}")
            check(seg_split.setdefault(sid, split) == split, f"segment {sid} crosses splits")
            segments_by_id.setdefault(sid, []).append(seg)
            for i, t in enumerate(seg["turns"]):
                check(t["original_index"] == i + 1,
                      f"turn {t['id']} original_index not 1-based sequential in {sid}/{src['transcript_revision']}")
                key = (sid, src["transcript_revision"], t["id"])
                check(key not in sources, f"duplicate source turn {key}")
                sources[key] = cid
                turn_by_key[key] = t

    ids: set[str] = set()
    normalized: set[str] = set()
    group_questions: dict[str, list] = {}
    code_by_group: dict[str, str] = {}
    split_of_group: dict[str, str] = {}
    label_counts = {"complete": 0, "none": 0, "partial": 0}
    for q in questions:
        qid = q.get("id") or ""
        check(qid and qid not in ids, f"missing or duplicate question id {qid!r}")
        ids.add(qid)
        group, split, bucket = q["group_id"], q["split"], q["bucket"]
        check(bucket in BUCKETS, f"{qid} has unknown bucket {bucket!r}")
        check(split in SPLITS, f"{qid} has unknown split {split!r}")
        check(split_of_group.setdefault(group, split) == split, f"group {group} crosses splits")
        check(q["source_group_id"] == group, f"{qid} source_group_id mismatch")
        norm = "".join(q["query"].split()).casefold()
        check(bool(norm) and CJK.search(norm) is not None, f"{qid} query is empty or non-Chinese")
        check(norm not in normalized, f"{qid} duplicate query text")
        normalized.add(norm)
        runs = CODE_RE.findall(q["query"])
        check(len(runs) == 1, f"{qid} must carry exactly one numeric code literal, got {runs}")
        if runs:
            check(code_by_group.setdefault(group, runs[0]) == runs[0],
                  f"group {group} queries disagree on code literal")
        group_questions.setdefault(group, []).append(q)
        refs = q["gold_turn_refs"]
        for ref in refs:
            key = (ref["segment_id"], ref["transcript_revision"], ref["turn_id"])
            check(key in sources, f"{qid} references missing source {key}")
            if key in sources:
                check(conv_split.get(sources[key]) == split, f"{qid} references a source from another split")
                ref_base = sources[key][2:]
                m = SESSION_SUFFIX_RE.fullmatch(ref_base)
                ref_group = m.group(1) if m else ref_base
                check(ref_group == group, f"{qid} references another group's turn {key}")
        comp, exp = q["evidence_completeness"], q["expected_sufficiency"]
        label_counts[comp] = label_counts.get(comp, 0) + 1
        if comp == "complete":
            check(not q["unanswerable"] and exp == "sufficient" and refs,
                  f"{qid} complete label invalid")
            if bucket == "cross_session":
                convs_cited = {sources[(r["segment_id"], r["transcript_revision"], r["turn_id"])] for r in refs if
                               (r["segment_id"], r["transcript_revision"], r["turn_id"]) in sources}
                check(len(convs_cited) >= 2, f"{qid} cross_session must cite two conversations")
            if bucket == "multi_source":
                check(len(refs) >= 2, f"{qid} multi_source must cite at least two turns")
        elif comp == "none":
            check(q["unanswerable"] and exp == "insufficient" and not refs and q["gold_answer"] is None,
                  f"{qid} none label invalid")
        elif comp == "partial":
            check(not q["unanswerable"] and exp == "insufficient" and bucket in PARTIAL_BUCKETS
                  and len(refs) == 1, f"{qid} partial label invalid")
        else:
            check(False, f"{qid} invalid evidence_completeness {comp!r}")
        if comp == "complete" and bucket in TOP1_BUCKETS:
            check(len(refs) == 1 and q.get("gold_top1_turn_ref") == refs[0],
                  f"{qid} top1 must equal its single gold ref")
        else:
            check("gold_top1_turn_ref" not in q, f"{qid} must not carry gold_top1_turn_ref")
        check(q.get("expect_ambient_excluded") is True and q.get("expect_stale_excluded") is True,
              f"{qid} exclusion flags must be true")
        check(q.get("scope") == {}, f"{qid} scope must be empty")

    bucket_groups = {b: {s: set() for s in SPLITS} for b in BUCKETS}
    for group, items in group_questions.items():
        bucket = items[0]["bucket"]
        want = 3 if bucket in PARTIAL_BUCKETS else 2
        check(len(items) == want, f"group {group} has {len(items)} questions, want {want}")
        labels = sorted(i["evidence_completeness"] for i in items)
        check(labels.count("complete") == 1 and labels.count("none") == 1
              and labels.count("partial") == want - 2, f"group {group} composition invalid {labels}")
        bucket_groups[bucket][items[0]["split"]].add(group)

    for b in BUCKETS:
        for s in SPLITS:
            check(len(bucket_groups[b][s]) == GROUPS_PER_BUCKET_SPLIT,
                  f"bucket {b}/{s} has {len(bucket_groups[b][s])} groups, want 10")
    split_groups = {s: sum(1 for v in split_of_group.values() if v == s) for s in SPLITS}
    split_questions = {s: sum(1 for q in questions if q["split"] == s) for s in SPLITS}
    check(len(group_questions) == 330, f"total groups {len(group_questions)} != 330")
    check(len(questions) == 780, f"total questions {len(questions)} != 780")
    check(split_groups == {"train": 110, "calibration": 110, "test": 110},
          f"split groups invalid: {split_groups}")
    check(split_questions == {"train": 260, "calibration": 260, "test": 260},
          f"split questions invalid: {split_questions}")
    check(len(set(code_by_group.values())) == len(code_by_group),
          "group code literals must be unique across groups")

    # Old/new revision structure for time_change / revocation.
    for group, items in group_questions.items():
        bucket = items[0]["bucket"]
        if bucket in ("time_change", "revocation"):
            seg_id = f"seg-{group}"
            segs = segments_by_id.get(seg_id, [])
            revs = {s["source"]["transcript_revision"] for s in segs}
            check(revs == {"r1", "r2"}, f"{group} needs r1 and r2 segments")
            old_keys = [k for k in sources if k[0] == seg_id and k[1] == "r1"]
            cur_keys = [k for k in sources if k[0] == seg_id and k[1] == "r2"]
            check([k[2] for k in old_keys] == ["old"] and [k[2] for k in cur_keys] == ["current"],
                  f"{group} old/current turn ids malformed")
            if old_keys and cur_keys:
                old_text, cur_text = turn_by_key[old_keys[0]]["text"], turn_by_key[cur_keys[0]]["text"]
                check(old_text != cur_text, f"{group} old/current facts identical")
                check(code_by_group[group] in old_text and code_by_group[group] in cur_text,
                      f"{group} old/current turns must carry the group code")
                comp_q = next(i for i in items if i["evidence_completeness"] == "complete")
                got = {(r["segment_id"], r["transcript_revision"], r["turn_id"])
                       for r in comp_q["gold_turn_refs"]}
                check(got == {(seg_id, "r2", "current")},
                      f"{group} complete gold must point at the current turn only")
        if bucket == "wrong_identity_proposal":
            guest = [t for k, t in turn_by_key.items()
                     if k[0] == f"seg-{group}" and t["speaker_scoped_id"] != "speaker:host"]
            check(bool(guest) and guest[0]["speaker_entity_id"] is None,
                  f"{group} proposal speaker must be unconfirmed")
            check(any("提议" in t["text"] and "尚未" in t["text"] for k, t in turn_by_key.items()
                      if k[0] == f"seg-{group}"), f"{group} proposal text missing")
            comp_q = next(i for i in items if i["evidence_completeness"] == "complete")
            check("尚未确认" in (comp_q["gold_answer"] or "")
                  and "已确认" not in (comp_q["gold_answer"] or ""),
                  f"{group} complete gold answer must not assert identity confirmation")
        if bucket == "numeric_constraint":
            comp_q = next(i for i in items if i["evidence_completeness"] == "complete")
            check("二十" in comp_q["query"], f"{group} numeric query lacks the constraint literal")
            for r in comp_q["gold_turn_refs"]:
                key = (r["segment_id"], r["transcript_revision"], r["turn_id"])
                check(key in turn_by_key and "二十" in turn_by_key[key]["text"],
                      f"{group} numeric literal missing from cited turn text")

    # Lifecycle rules.
    check(lifecycle.get("schema") == "retrieval-lifecycle-v1", "lifecycle schema invalid")
    lc = lifecycle.get("sources", [])
    rev_groups = sorted(g for g in group_questions if g.startswith("rev-"))
    check(len(rev_groups) == 30, f"revocation groups {len(rev_groups)} != 30")
    check({f"seg-{g}" for g in rev_groups} == {i.get("segment_id") for i in lc},
          "lifecycle must cover exactly the revocation groups' old turns")
    seen_lc: set[tuple] = set()
    for item in lc:
        key = (item.get("segment_id"), item.get("transcript_revision"), item.get("turn_id"))
        check(key in sources, f"lifecycle source {key} missing from corpus")
        check(key not in seen_lc, f"duplicate lifecycle source {key}")
        seen_lc.add(key)
        check(item.get("transcript_revision") == "r1" and item.get("turn_id") == "old",
              f"lifecycle source {key} must be the r1 old turn")
        check(bool(item.get("reason")), f"lifecycle source {key} lacks reason")
        if key in turn_by_key:
            check(code_by_group.get(key[0][4:], "") in turn_by_key[key]["text"],
                  f"lifecycle old turn {key} is not lexically tied to its group's queries")
    for group in rev_groups:
        for q in group_questions[group]:
            check(q.get("expect_forgotten_excluded") is True,
                  f"{q['id']} must set expect_forgotten_excluded")

    if problems:
        raise ValueError("self-check failed:\n- " + "\n- ".join(problems))
    return {
        "groups": len(group_questions),
        "questions": len(questions),
        "complete": label_counts["complete"],
        "none": label_counts["none"],
        "partial": label_counts["partial"],
        "split_groups": split_groups,
        "split_questions": split_questions,
        "conversations": len(conversations),
        "source_turns": len(sources),
        "lifecycle_sources": len(lc),
    }


def _print_summary(s: dict) -> None:
    print(f"groups={s['groups']} questions={s['questions']} "
          f"complete={s['complete']} none={s['none']} partial={s['partial']}")
    print(f"split_groups={s['split_groups']}")
    print(f"split_questions={s['split_questions']}")
    print(f"conversations={s['conversations']} source_turns={s['source_turns']} "
          f"lifecycle_sources={s['lifecycle_sources']}")


# ---------------------------------------------------------------------------
# Frozen docs
# ---------------------------------------------------------------------------

SELF_CHECK_COMMAND = "python3 scripts/generate-quality-v3-fixture.py --out fixtures/eval/quality-v3-r1 --self-check"


def authoring_md(s: dict) -> str:
    rows = "\n".join(
        f"| {b} | {BUCKET_NOTES[b]} | 2 | 60 |"
        if b not in PARTIAL_BUCKETS else
        f"| {b} | {BUCKET_NOTES[b]} | 3（含 1 个 partial） | 90 |"
        for b in BUCKETS
    )
    return f"""# Retrieval quality v3 r1 authoring notes

本目录的 corpus.json / retrieval.json / lifecycle.json 全部由确定性模板生成器合成，
生成命令：`python3 scripts/generate-quality-v3-fixture.py --out fixtures/eval/quality-v3-r1`。

- 话轮与查询都是模板生成的合成文本，用于 evidence admission / assessor / lifecycle 的**协议校准**；
- **不是人工裁定（human-adjudicated）的真实数据**，不能作为真实检索质量或模型语义能力的证据；
- 全部人物、地点与事件均为虚构，不含任何私人录音或生产数据；
- 生成过程不含时间戳与随机数，重跑输出逐字节一致，SHA256SUMS.json 固定三个 JSON 的哈希。

## Bucket composition

| bucket | 说明 | 每组问题数 | 该 bucket 总问题数 |
|---|---|---|---|
{rows}

## Totals

- 组：{s['groups']}（train/calibration/test 各 110；11 个 bucket × 10 组 × 3 split）。
- 问题：{s['questions']}（每 split 260 = complete 110 + none 110 + partial 40）；
  全量 complete {s['complete']}、none {s['none']}、partial {s['partial']}。
- partial 仅出现在 time_change / revocation / missing_evidence / wrong_identity_proposal。
- lifecycle.json：{s['lifecycle_sources']} 条来源，全部为 revocation 组的 r1 旧话轮。

## Conventions

- 每组唯一锚点：`编号{{code}}`，code 由组序号决定（101..430），出现在事实话轮与该组全部问题文本中；
  查询中的三位数字字面量即该组唯一编号。
- time_change / revocation 组为同一 segment 的 r1/r2 两个修订（old/current 冲突事实），
  complete 金标只指向 r2 当前话轮。
- cross_session 组由两个独立会话（两个 conversation）组成，complete 金标各引用其一。
- 标签约定：complete=unanswerable=false/expected_sufficiency=sufficient；none=unanswerable=true/
  expected_sufficiency=insufficient 且无金标引用；partial=unanswerable=false/
  expected_sufficiency=insufficient，可引用一个相关话轮。
- revocation 组三个问题均带 `expect_forgotten_excluded: true`。
"""


def review_md(s: dict) -> str:
    return f"""# v3 r1 冻结前自检记录

以下各项由生成器自检程序逐项确认（生成器结构自检，非人工语义审查）。
自检命令：`{SELF_CHECK_COMMAND}`

- [x] 组与问题总数：{s['groups']} 组、{s['questions']} 题；train/calibration/test 各 110 组、260 题；
  每 bucket × split 恰 10 组。
- [x] 组内构成：每组 1 个 complete + 1 个 none；partial 仅出现于
  time_change/revocation/missing_evidence/wrong_identity_proposal（每 split 40 题、全量 {s['partial']} 题）。
- [x] 分区隔离：group_id / source_group_id / segment_id / conversation_id / 金标引用均不跨 split。
- [x] 问题唯一性：strip+casefold 后 {s['questions']} 条查询互不重复，均为中文，且每组查询携带唯一的三位编号字面量
  （组间唯一，同时出现在金标话轮文本中）。
- [x] cross_session complete 问题引用两个不同会话；multi_source complete 问题引用至少两个话轮。
- [x] time_change / revocation 组含 r1 旧话轮与 r2 当前话轮的冲突事实；complete 金标只指向当前话轮。
- [x] wrong_identity_proposal 组含未确认 speaker（speaker_entity_id 为空）的提议话轮；
  complete 金标答案只报告“尚未确认”，不断言身份已确认。
- [x] numeric_constraint 查询中的数字（二十）出现在其话轮文本中。
- [x] lifecycle.json：{s['lifecycle_sources']} 条来源，全部为 revocation 组的 r1 旧话轮并带 reason；
  对应组三个问题均带 `expect_forgotten_excluded`；旧话轮文本与组查询共享编号与场地，遗忘缺失可观察。
- [x] SHA256SUMS.json 覆盖 corpus.json / retrieval.json / lifecycle.json，与文件实际哈希一致。

本记录仅证明生成器结构自检通过；不代表任何模型质量门槛通过，也不替代人工语义审查。
"""


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(
        description="Generate the deterministic retrieval-quality-v3 fixture (r1).")
    parser.add_argument("--out", default="",
                        help="fixture directory to write (required unless --self-check)")
    parser.add_argument("--self-check", action="store_true",
                        help="validate the generated fixture in memory and exit without writing")
    args = parser.parse_args()

    corpus, retrieval, lifecycle = build_fixture()
    summary = self_check(corpus, retrieval, lifecycle)

    if args.self_check:
        _print_summary(summary)
        print("self-check OK: counts, composition, split isolation and lifecycle rules all passed")
        return 0
    if not args.out:
        parser.error("--out is required unless --self-check")

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    docs = {"corpus.json": corpus, "retrieval.json": retrieval, "lifecycle.json": lifecycle}
    hashes = {}
    for name, doc in docs.items():
        data = json.dumps(doc, ensure_ascii=False, indent=2) + "\n"
        path = out / name
        path.write_text(data, encoding="utf-8")
        hashes[name] = hashlib.sha256(data.encode("utf-8")).hexdigest()
        print(f"wrote {path} ({path.stat().st_size} bytes)")
    sums = out / "SHA256SUMS.json"
    sums.write_text(json.dumps(hashes, indent=2) + "\n", encoding="utf-8")
    print(f"wrote {sums} ({sums.stat().st_size} bytes)")
    (out / "AUTHORING.md").write_text(authoring_md(summary), encoding="utf-8")
    (out / "REVIEW.md").write_text(review_md(summary), encoding="utf-8")

    # Verify the written fixture from disk: round-trip, hashes and full rules.
    for name, doc in docs.items():
        if json.loads((out / name).read_text(encoding="utf-8")) != doc:
            raise ValueError(f"{name} round-trip mismatch")
    manifest = json.loads((out / "SHA256SUMS.json").read_text(encoding="utf-8"))
    if set(manifest) != set(docs):
        raise ValueError("SHA256SUMS must pin exactly corpus.json, retrieval.json and lifecycle.json")
    for name in docs:
        if hashlib.sha256((out / name).read_bytes()).hexdigest() != manifest[name]:
            raise ValueError(f"SHA256SUMS mismatch for {name}")
    self_check(json.loads((out / "corpus.json").read_text(encoding="utf-8")),
               json.loads((out / "retrieval.json").read_text(encoding="utf-8")),
               json.loads((out / "lifecycle.json").read_text(encoding="utf-8")))
    _print_summary(summary)
    print(f"self-check OK: written fixture at {out} verified against SHA256SUMS.json")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

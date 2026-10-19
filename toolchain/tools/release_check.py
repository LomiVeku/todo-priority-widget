#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""release_check.py —— 交付体检脚本（清单：output/交付规范与体检清单-20261019.md）

v1.1（2026-10-19，依 space bunny 独立检验意见修订）
  M1 阻断修复：SKIP 为第三态（ok=None），verdict=PARTIAL，不得输出 PASS；exit 3（--allow-partial 才放行）
  M2：RC-05 增加断言数下限（profile.min_selftest_checks）
  M4：RC-05 检查自检进程退出码
  M3：留档 JSON 在 RC-08 入列后写入（含全部检查项）
  M5：自检调用异常兜底（不崩溃，结构化 FAIL）
  M7：RC-02 双向校验（ico 帧 ⊆ exe 帧 + exe 内帧去重）
  M8：--selftest-image 缺失 → 显式 FAIL（不再静默降级）
  + RC-07 独立职责（0 字节 / 自检残留 / 隐藏属性）
  + RC-09 可选 exe 哈希基准（识别「资源正确但代码陈旧」）
  + 版本登记表半自动字符串比对（WARN，不阻断）
  + VERSIONINFO 语种枚举（不再硬编码 040904B0）

用法：
  python toolchain/tools/release_check.py --profile internal \
      --dir "output/待办事项优先级提示窗-Go版" --expect-version 1.2.1

退出码：0 = PASS；1 = FAIL；2 = 参数/环境错误；3 = PARTIAL（有 SKIP，门禁需 --allow-partial 才放行）
"""
import argparse
import ctypes
import hashlib
import json
import os
import struct
import subprocess
import sys
from ctypes import wintypes
from datetime import datetime

CHECKLIST_VERSION = 'v1.1-r2'

PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..'))

PROFILES = {
    'internal': {
        'exe': '待办事项优先级提示窗-Go.exe',
        'required': [
            '待办事项优先级提示窗-Go.exe',
            os.path.join('assets', 'app.js'),
            os.path.join('assets', 'icon.ico'),
            os.path.join('assets', 'index.html'),
            os.path.join('assets', 'styles.css'),
            os.path.join('assets', 'src', 'main', 'parser.js'),
            os.path.join('assets', 'src', 'shared', 'meta.js'),
        ],
        'assets_src': os.path.join(PROJECT_ROOT, 'app-go', 'assets'),
        'min_icon_frames': 7,
        'min_selftest_checks': 124,      # M2：断言数下限（1.3.0 T6 起 124/0；旧值 110）
        'allowed_extra': ['SHA256SUMS.txt'],
    },
}

DEFAULT_SELFTEST_IMAGE = os.path.join(PROJECT_ROOT, '_verify', 'corner-fix-run', 'testimg.png')
VERSION_REGISTRY = os.path.join(PROJECT_ROOT, '版本登记-内部.md')

SELFTEST_RESIDUE_HINTS = ('selftest-report.json',)
SELFTEST_RESIDUE_EXTS = ('.log',)


# ---------------------------------------------------------------- 基础工具

def sha256_file(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def parse_ico(path):
    d = open(path, 'rb').read()
    if len(d) < 6:
        raise ValueError('ICO 文件过短')
    reserved, typ, count = struct.unpack_from('<HHH', d, 0)
    if typ != 1 or count == 0:
        raise ValueError('不是标准 ICO（type=%d count=%d）' % (typ, count))
    frames = []
    for i in range(count):
        off = 6 + i * 16
        w, h, cc, r, planes, bpp, size, imageoff = struct.unpack_from('<BBBBHHII', d, off)
        blob = d[imageoff:imageoff + size]
        if len(blob) != size:
            raise ValueError('ICO 帧 %d 数据越界' % i)
        frames.append({'w': w or 256, 'h': h or 256, 'bpp': bpp,
                       'size': size, 'sha256': hashlib.sha256(blob).hexdigest()})
    return frames


def parse_pe_resources(path):
    data = open(path, 'rb').read()
    if data[:2] != b'MZ':
        raise ValueError('不是 PE 文件（无 MZ 头）')
    e_lfanew = struct.unpack_from('<I', data, 0x3C)[0]
    if data[e_lfanew:e_lfanew + 4] != b'PE\0\0':
        raise ValueError('不是 PE 文件（无 PE 签名）')
    coff = e_lfanew + 4
    num_sections = struct.unpack_from('<H', data, coff + 2)[0]
    size_opt = struct.unpack_from('<H', data, coff + 16)[0]
    opt = coff + 20
    magic = struct.unpack_from('<H', data, opt)[0]
    dd_off = opt + (0x60 if magic == 0x10b else 0x70)
    rsrc_rva, rsrc_size = struct.unpack_from('<II', data, dd_off + 2 * 8)
    if rsrc_rva == 0:
        return {'icons': [], 'groups': []}
    sec = opt + size_opt
    sections = []
    for i in range(num_sections):
        off = sec + i * 40
        vsize, vaddr, rsize, roff = struct.unpack_from('<IIII', data, off + 8)
        sections.append((vaddr, max(rsize, vsize), roff))

    def rva2off(rva):
        for vaddr, size, roff in sections:
            if vaddr <= rva < vaddr + size:
                return roff + (rva - vaddr)
        return None

    base = rva2off(rsrc_rva)
    if base is None:
        raise ValueError('资源节无法映射到文件偏移')
    out = {'icons': [], 'groups': []}

    def walk(dir_off, depth, type_id):
        if depth > 3:
            return
        d = base + dir_off
        named = struct.unpack_from('<H', data, d + 12)[0]
        idents = struct.unpack_from('<H', data, d + 14)[0]
        for i in range(named + idents):
            e = d + 16 + i * 8
            name_id = struct.unpack_from('<I', data, e)[0]
            off_field = struct.unpack_from('<I', data, e + 4)[0]
            if off_field & 0x80000000:
                walk(off_field & 0x7FFFFFFF, depth + 1, name_id if depth == 0 else type_id)
            else:
                de = base + (off_field & 0x7FFFFFFF)
                data_rva, data_size, cp, res = struct.unpack_from('<IIII', data, de)
                foff = rva2off(data_rva)
                if foff is None:
                    continue
                h = hashlib.sha256(data[foff:foff + data_size]).hexdigest()
                if type_id == 3:
                    out['icons'].append((name_id & 0xFFFF, data_size, h))
                elif type_id == 14:
                    out['groups'].append((name_id & 0xFFFF, data_size, h))

    walk(0, 0, None)
    return out


def read_version_info(path):
    """读 VS_VERSIONINFO（语种从 VarFileInfo\\Translation 动态取，不硬编码）"""
    size = ctypes.windll.version.GetFileVersionInfoSizeW(path, None)
    if not size:
        return {}
    buf = ctypes.create_string_buffer(size)
    if not ctypes.windll.version.GetFileVersionInfoW(path, 0, size, buf):
        return {}
    result = {}
    fixed = ctypes.c_void_p()
    flen = wintypes.UINT()
    if ctypes.windll.version.VerQueryValueW(buf, '\\', ctypes.byref(fixed), ctypes.byref(flen)):
        ffi = ctypes.cast(fixed, ctypes.POINTER(wintypes.DWORD * 13)).contents
        ms, ls = ffi[2], ffi[3]
        result['FixedFileVersion'] = '%d.%d.%d.%d' % (ms >> 16, ms & 0xFFFF,
                                                       ls >> 16, ls & 0xFFFF)
    # 语种枚举
    trans = ctypes.c_void_p()
    tlen = wintypes.UINT()
    lang_sub = '040904B0'
    if ctypes.windll.version.VerQueryValueW(buf, '\\VarFileInfo\\Translation',
                                            ctypes.byref(trans), ctypes.byref(tlen)) and trans:
        if tlen.value >= 4:
            raw = ctypes.string_at(trans, 4)
            lang, cp = struct.unpack('<HH', raw)
            lang_sub = '%04x%04x' % (lang, cp)
    for key in ('FileVersion', 'ProductVersion', 'FileDescription',
                'ProductName', 'OriginalFilename'):
        sub = '\\StringFileInfo\\' + lang_sub + '\\' + key
        pv = ctypes.c_void_p()
        ln = wintypes.UINT()
        if ctypes.windll.version.VerQueryValueW(buf, sub, ctypes.byref(pv), ctypes.byref(ln)) and pv:
            raw = ctypes.wstring_at(pv, ln.value).rstrip('\0')  # ln 单位=字符
            if raw:
                result[key] = raw
    return result


def walk_rel_files(root):
    found = {}
    for dirpath, dirnames, filenames in os.walk(root):
        for fn in filenames:
            full = os.path.join(dirpath, fn)
            rel = os.path.relpath(full, root).replace('\\', '/')
            found[rel] = full
    return found


def norm(p):
    return p.replace('\\', '/')


# ---------------------------------------------------------------- 检查项

def rc01_structure(target, profile):
    required = {norm(p) for p in profile['required']}
    allowed = {norm(p) for p in profile.get('allowed_extra', [])}
    actual = set(walk_rel_files(target).keys())
    missing = sorted(required - actual)
    extra = sorted(actual - required - allowed)
    if missing or extra:
        parts = []
        if missing:
            parts.append('缺少: ' + ', '.join(missing))
        if extra:
            parts.append('多余: ' + ', '.join(extra))
        return False, '；'.join(parts)
    return True, '%d 个必需文件齐全，无多余文件' % len(required)


def rc02_icon(exe_path, assets_ico, min_frames):
    if not os.path.exists(assets_ico):
        return False, '基准图标不存在: %s' % assets_ico
    frames = parse_ico(assets_ico)
    ref = {f['sha256'] for f in frames}
    res = parse_pe_resources(exe_path)
    icons = res['icons']
    if not icons:
        return False, 'exe 无 RT_ICON 资源（裸 Go 二进制不允许交付）'
    if len(icons) < min_frames:
        return False, 'RT_ICON 帧数 %d < 要求 %d' % (len(icons), min_frames)
    exe_hashes = [h for _, _, h in icons]
    if len(set(exe_hashes)) != len(exe_hashes):
        return False, 'exe 内 RT_ICON 存在重复帧（%d 条 / 去重后 %d）' % (
            len(exe_hashes), len(set(exe_hashes)))
    hit = sum(1 for h in exe_hashes if h in ref)
    if hit != len(exe_hashes):
        return False, '帧匹配 %d/%d（有不匹配帧）' % (hit, len(exe_hashes))
    # M7 反向：基准 ico 的每一帧都必须在 exe 中出现（防丢帧）
    missing = ref - set(exe_hashes)
    if missing:
        return False, '基准 ico 有 %d 帧未出现在 exe 中' % len(missing)
    if len(res['groups']) == 0:
        return False, '有 RT_ICON 但无 RT_GROUP_ICON'
    return True, 'RT_ICON=%d 全帧双向匹配，RT_GROUP_ICON=%d' % (len(icons), len(res['groups']))


def rc03_version(exe_path, expect):
    v = read_version_info(exe_path)
    if not v:
        return False, 'exe 无 VS_VERSIONINFO'
    fv = v.get('FileVersion', '')
    pv = v.get('ProductVersion', '')
    # N1（sb 复核）：恢复精确相等——子串匹配会让 expect=2.1 在 1.2.1 上误通过。
    # 注：对外版（1.0beta，FileVersion=1.0.0.0 / ProductVersion=1.0beta）属未实现 profile，
    #     届时在 profile 内声明版本比较规则，不得全局放宽。
    ok = (fv == expect and pv == expect)
    return ok, 'FileVersion=%s ProductVersion=%s（期望 %s，两者均须精确相等）' % (fv, pv, expect)


def rc04_assets(target_assets, src_assets):
    if not os.path.isdir(src_assets):
        return False, '源 assets 目录不存在: %s' % src_assets
    src_files = set(walk_rel_files(src_assets).keys())
    dst_files = set(walk_rel_files(target_assets).keys())
    if src_files != dst_files:
        diff = sorted(src_files ^ dst_files)
        return False, '文件集合不一致: ' + ', '.join(diff)
    bad = []
    for rel in sorted(src_files):
        h1 = sha256_file(os.path.join(src_assets, rel.replace('/', os.sep)))
        h2 = sha256_file(os.path.join(target_assets, rel.replace('/', os.sep)))
        if h1 != h2:
            bad.append(rel)
    if bad:
        return False, '哈希不一致: ' + ', '.join(bad)
    return True, '%d 个文件与源目录 SHA256 全一致（注意：一致性检查≠完整性，基准仍是源目录）' % len(src_files)


def rc05_selftest(exe_path, image, log_dir, min_checks):
    """返回 (ok: True/False/None, detail, report)。None=SKIP（调用方决定）"""
    if not os.path.exists(exe_path):
        return False, 'exe 不存在', None
    if not os.path.exists(image):
        # M8：图片缺失显式 FAIL，不静默降级
        return False, '自检图片不存在: %s（不允许静默省略）' % image, None
    os.makedirs(log_dir, exist_ok=True)
    cmd = [exe_path, '--selftest', '--selftest-out', log_dir, '--selftest-image', image]
    try:
        p = subprocess.run(cmd, capture_output=True, timeout=300,
                           cwd=os.path.dirname(exe_path))
    except subprocess.TimeoutExpired:
        return False, '自检超时（>300s）', None
    except Exception as e:  # M5
        return False, '自检启动异常: %s' % e, None
    # M4：退出码判定
    if p.returncode != 0:
        return False, '自检进程退出码 %d（非 0 一律 FAIL）' % p.returncode, None
    rp = os.path.join(log_dir, 'selftest-report.json')
    if not os.path.exists(rp):
        return False, '未生成自检报告', None
    try:
        report = json.load(open(rp, encoding='utf-8'))
    except Exception as e:  # M5
        return False, '自检报告损坏: %s' % e, None
    checks = report.get('checks', [])
    if not checks:
        return False, '自检报告无 checks', report
    # M2：断言数下限
    if len(checks) < min_checks:
        return False, '断言项数 %d < 下限 %d（疑似旧版 exe）' % (len(checks), min_checks), report
    ok = sum(1 for c in checks if c.get('ok'))
    if ok != len(checks):
        failed = [c.get('name') for c in checks if not c.get('ok')]
        return False, '%d/%d 通过，失败: %s' % (ok, len(checks), ', '.join(failed)), report
    return True, '%d/%d 全通过（下限 %d）' % (ok, len(checks), min_checks), report


def rc06_version_consistency(exe_version, selftest_report, expect, selftest_skipped=False):
    if not exe_version:
        return False, '无 VERSIONINFO，无法比对'
    fv = exe_version.get('FileVersion', '')
    pv = exe_version.get('ProductVersion', '')
    ui = ''
    if selftest_report:
        for c in selftest_report.get('checks', []):
            if c.get('name') == 'version-string':
                ui = str(c.get('detail', ''))
                break
    ok_file = (fv == expect and pv == expect)  # N1：精确相等
    if selftest_skipped and not ui:
        return ok_file, '文件=%s/%s / 界面=SKIPPED / 期望=%s（RC-05 已 SKIP，整体为 PARTIAL）' % (
            fv, pv, expect)
    parts = ['文件=%s/%s' % (fv, pv), '界面=%s' % (ui or '(未取到)'), '期望=%s' % expect]
    ok = ok_file and (ui == expect)
    return ok, ' / '.join(parts)


def rc07_cleanliness(target, profile):
    """独立职责：0 字节文件 / 自检残留 / 隐藏或系统属性文件"""
    problems = []
    files = walk_rel_files(target)
    for rel, full in sorted(files.items()):
        try:
            st = os.stat(full)
        except OSError as e:
            problems.append('%s 无法 stat(%s)' % (rel, e))
            continue
        if st.st_size == 0:
            problems.append('0 字节: ' + rel)
        base = os.path.basename(rel)
        if base in SELFTEST_RESIDUE_HINTS or base.lower().endswith(SELFTEST_RESIDUE_EXTS):
            problems.append('自检残留: ' + rel)
        attrs = getattr(st, 'st_file_attributes', 0)
        FILE_ATTRIBUTE_HIDDEN = 0x2
        FILE_ATTRIBUTE_SYSTEM = 0x4
        if attrs & (FILE_ATTRIBUTE_HIDDEN | FILE_ATTRIBUTE_SYSTEM):
            problems.append('隐藏/系统属性: ' + rel)
    if problems:
        return False, '；'.join(problems[:8]) + ('' if len(problems) <= 8 else ' …共%d项' % len(problems))
    return True, '无 0 字节/自检残留/隐藏属性文件（%d 项）' % len(files)


def rc09_exe_hash(exe_path, expect_sha):
    if not os.path.exists(exe_path):
        return False, 'exe 不存在'
    actual = sha256_file(exe_path)
    if actual.lower() != expect_sha.lower():
        return False, 'exe 哈希不符：实际 %s… / 基准 %s…' % (actual[:16], expect_sha[:16])
    return True, 'exe 哈希与基准一致（%s…）' % actual[:16]


# ---------------------------------------------------------------- 主流程

def main():
    ap = argparse.ArgumentParser(description='交付体检（release-check %s）' % CHECKLIST_VERSION)
    ap.add_argument('--profile', required=True, choices=sorted(PROFILES))
    ap.add_argument('--dir', required=True)
    ap.add_argument('--expect-version', required=True)
    ap.add_argument('--expect-exe-sha256', default=None,
                    help='可选：exe 哈希基准（识别「资源正确但代码陈旧」，推荐从构建输出/版本登记取）')
    ap.add_argument('--skip-selftest', action='store_true',
                    help='跳过 RC-05（结果为 SKIP，整体 verdict=PARTIAL，exit 3，除非 --allow-partial）')
    ap.add_argument('--allow-partial', action='store_true', help='PARTIAL 也视为可接受（exit 0）')
    ap.add_argument('--selftest-image', default=DEFAULT_SELFTEST_IMAGE)
    ap.add_argument('--json-out', default=None)
    args = ap.parse_args()

    profile = PROFILES[args.profile]
    target = os.path.abspath(args.dir)
    if not os.path.isdir(target):
        print('ERROR: 目录不存在: %s' % target)
        return 2

    results = []

    def add(rc_id, name, ok, detail):
        results.append({'id': rc_id, 'name': name, 'ok': ok, 'detail': detail})
        tag = 'PASS' if ok is True else ('SKIP' if ok is None else 'FAIL')
        print('  [%s] %-4s %-14s %s' % (tag, rc_id, name, detail))

    print('release-check %s | profile=%s | target=%s' % (CHECKLIST_VERSION, args.profile, target))
    print('期望版本: %s%s' % (args.expect_version,
                            '' if args.expect_exe_sha256 else '（未提供 exe 哈希基准，RC-09 未启用）'))
    print('-' * 78)

    exe_path = os.path.join(target, profile['exe'])
    assets = os.path.join(target, 'assets')

    add('RC-01', '目录结构', *rc01_structure(target, profile))

    try:
        add('RC-02', '图标资源', *rc02_icon(exe_path, os.path.join(assets, 'icon.ico'),
                                          profile['min_icon_frames']))
    except Exception as e:
        add('RC-02', '图标资源', False, '解析异常: %s' % e)

    exe_version = {}
    try:
        ok, detail = rc03_version(exe_path, args.expect_version)
        exe_version = read_version_info(exe_path)
    except Exception as e:
        ok, detail = False, '解析异常: %s' % e
    add('RC-03', '版本资源', ok, detail)

    try:
        add('RC-04', 'assets一致性', *rc04_assets(assets, profile['assets_src']))
    except Exception as e:
        add('RC-04', 'assets一致性', False, '校验异常: %s' % e)

    selftest_report = None
    if args.skip_selftest:
        add('RC-05', '应用内自检', None, 'SKIPPED（--skip-selftest，整体 PARTIAL）')  # M1 第三态
    else:
        log_dir = os.path.join(PROJECT_ROOT, '_verify',
                               'release-check-%s' % datetime.now().strftime('%Y%m%d-%H%M%S'))
        try:
            ok, detail, selftest_report = rc05_selftest(
                exe_path, args.selftest_image, log_dir, profile['min_selftest_checks'])
        except Exception as e:  # M5：任何异常都结构化 FAIL
            ok, detail = False, '自检调用异常: %s' % e
        add('RC-05', '应用内自检', ok, detail)

    try:
        add('RC-06', '版本三方一致',
            *rc06_version_consistency(exe_version, selftest_report,
                                      args.expect_version, args.skip_selftest))
    except Exception as e:
        add('RC-06', '版本三方一致', False, '比对异常: %s' % e)

    try:
        add('RC-07', '干净性', *rc07_cleanliness(target, profile))
    except Exception as e:
        add('RC-07', '干净性', False, '校验异常: %s' % e)

    if args.expect_exe_sha256:
        try:
            add('RC-09', 'exe哈希基准', *rc09_exe_hash(exe_path, args.expect_exe_sha256))
        except Exception as e:
            add('RC-09', 'exe哈希基准', False, '比对异常: %s' % e)

    # ---- RC-08 留档：先写 SHA256SUMS，再入列，最后写 JSON（M3：保证 JSON 含全部项）
    sums_ok, sums_detail = False, ''
    try:
        allowed = {norm(p) for p in profile.get('allowed_extra', [])}
        actual = set(walk_rel_files(target).keys())
        sums = []
        for rel in sorted(actual - allowed):
            full = os.path.join(target, rel.replace('/', os.sep))
            sums.append('%s  %s' % (sha256_file(full), rel))
        sums_path = os.path.join(target, 'SHA256SUMS.txt')
        open(sums_path, 'w', encoding='utf-8', newline='\n').write('\n'.join(sums) + '\n')
        sums_ok = True
        sums_detail = 'SHA256SUMS.txt（%d 项）' % len(sums)
    except Exception as e:
        sums_detail = 'SHA256SUMS 写入失败: %s' % e
    add('RC-08', '留档产物', sums_ok, sums_detail)

    # verdict：False 优先 → FAIL；有 None → PARTIAL；全 True → PASS
    if any(r['ok'] is False for r in results):
        verdict = 'FAIL'
    elif any(r['ok'] is None for r in results):
        verdict = 'PARTIAL'
    else:
        verdict = 'PASS'

    json_ok = False
    try:
        report = {
            'checklist_version': CHECKLIST_VERSION,
            'profile': args.profile,
            'target_dir': target,
            'expect_version': args.expect_version,
            'timestamp': datetime.now().isoformat(timespec='seconds'),
            'exe_sha256': sha256_file(exe_path) if os.path.exists(exe_path) else None,
            'results': results,          # 已含 RC-08（及可选 RC-09）
            'verdict': verdict,
        }
        side_dir = os.path.join(PROJECT_ROOT, '_verify', 'release-check-reports')
        os.makedirs(side_dir, exist_ok=True)
        json_path = args.json_out or os.path.join(
            side_dir, '%s-%s.json' % (args.profile, datetime.now().strftime('%Y%m%d-%H%M%S')))
        json.dump(report, open(json_path, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)
        json_ok = True
        print('  留档: %s' % json_path)
    except Exception as e:
        # N2：清单 RC-08 要求「两文件均生成成功」——JSON 失败必须影响结论，不得静默降级
        print('  [FAIL] JSON 留档失败: %s' % e)

    # 版本登记表半自动比对（WARN，不阻断；N3：行级精确匹配，不用裸子串）
    try:
        if os.path.exists(VERSION_REGISTRY):
            lines = open(VERSION_REGISTRY, encoding='utf-8').read().splitlines()
            hit = any(('| **%s**' % args.expect_version) in L
                      or ('| %s |' % args.expect_version) in L
                      for L in lines)
            if not hit:
                print('  [WARN] 版本登记表未出现「%s」的表格行——请确认登记是否遗漏'
                      % args.expect_version)
    except Exception as e:
        print('  [WARN] 版本登记表检查跳过: %s' % e)

    print('-' * 78)
    print('结论: %s （%d 项：%d PASS / %d FAIL / %d SKIP）' % (
        verdict,
        len(results),
        sum(1 for r in results if r['ok'] is True),
        sum(1 for r in results if r['ok'] is False),
        sum(1 for r in results if r['ok'] is None)))
    if not json_ok:
        print('结论修正: FAIL —— 留档产物不完整（RC-08 要求 SHA256SUMS.txt + JSON 两文件均生成）')
        return 1
    if not args.expect_exe_sha256:
        print('提示: 未提供 --expect-exe-sha256 —— 「资源正确但代码陈旧」类缺陷无法拦截；正式交付门禁应显式提供基准哈希')
    if verdict == 'PARTIAL' and not args.allow_partial:
        print('提示: PARTIAL 不是 PASS——门禁场景请补跑自检（去掉 --skip-selftest）；确需放行加 --allow-partial')
        return 3
    return 0 if verdict in ('PASS', 'PARTIAL') else 1


if __name__ == '__main__':
    sys.exit(main())

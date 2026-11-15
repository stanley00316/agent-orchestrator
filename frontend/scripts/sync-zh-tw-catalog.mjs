#!/usr/bin/env node
// 重新產生繁體中文（台灣）語言檔 src/renderer/i18n/zh-TW.json
//
// 白話說明：
//   官方更新後，英文和簡體中文語言檔會有新句子，繁中檔不會自動跟上。
//   執行這支工具，就會用最新的簡中檔重新做出繁中檔，流程是：
//     1. 簡體中文 → OpenCC 轉成繁體字與台灣用語
//     2. 套用台灣軟體用語校正表（scripts/zh-tw/glossary.mjs）
//     3. 套用人工翻譯（scripts/zh-tw/overrides.json）——簡中檔裡還是英文的句子由這裡補上
//
// 用法（在 frontend/ 資料夾執行）：
//   npm run i18n:zh-tw            重新產生繁中檔與選單列文字
//   node scripts/sync-zh-tw-catalog.mjs --check
//                                 只檢查、不寫入；繁中檔過期時回傳失敗（exit code 1）
//
// 人工翻譯檔的格式：每句都記下當初翻譯時的英文原文。
//   "settings.coder.saved": { "en": "Saved", "zhTW": "已儲存" }
// 官方若改了英文，"en" 會對不上，工具就不再套用這句舊翻譯，並提醒你重翻。
// 想刻意保留英文（例如產品名稱），就讓 "zhTW" 和 "en" 一樣。
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as OpenCC from "opencc-js";
import { glossary } from "./zh-tw/glossary.mjs";

const frontend = join(dirname(fileURLToPath(import.meta.url)), "..");
const i18nDir = join(frontend, "src/renderer/i18n");
const overridesPath = join(frontend, "scripts/zh-tw/overrides.json");
const outputPath = join(i18nDir, "zh-TW.json");

// 每份語言檔都要有「繁體中文」選項名稱；英文檔用英文，其他語言用母語寫法
const OTHER_CATALOGS = ["en", "zh-CN", "ja", "ko", "es", "fr", "de", "pt-BR", "ru"];
const ZH_TW_LABEL_KEY = "settings.language.zhTW";
const zhTwLabelFor = (locale) => (locale === "en" ? "Traditional Chinese" : "繁體中文");

// 只接受已知的參數，打錯字時直接提醒，避免以為有檢查其實沒有
const args = process.argv.slice(2);
const unknownArgs = args.filter((arg) => arg !== "--check");
if (unknownArgs.length > 0) {
	console.error(`不認得的參數：${unknownArgs.join(" ")}（只支援 --check）`);
	process.exit(2);
}
const checkOnly = args.includes("--check");

const readJson = (path) => JSON.parse(readFileSync(path, "utf8"));
const en = readJson(join(i18nDir, "en.json"));
const zhCN = readJson(join(i18nDir, "zh-CN.json"));
const overrides = readJson(overridesPath);

// —— 步驟 1＋2：簡中 → 繁中台灣用語 → 校正表 ——
const toTraditional = OpenCC.Converter({ from: "cn", to: "twp" });
const convert = (text) => glossary.reduce((acc, [pattern, replacement]) => acc.replace(pattern, replacement), toTraditional(text));
const convertValue = (value) => (Array.isArray(value) ? value.map(convert) : convert(value));

// —— 步驟 3：套用人工翻譯（英文原文沒變才套用）——
const staleOverrides = [];
const obsoleteOverrides = [];
const translated = {};
for (const [key, value] of Object.entries(zhCN)) {
	const override = overrides[key];
	if (override && override.en === en[key]) {
		translated[key] = override.zhTW;
	} else {
		if (override && key in en) staleOverrides.push(key);
		translated[key] = convertValue(value);
	}
}
for (const key of Object.keys(overrides)) {
	if (!(key in en)) obsoleteOverrides.push(key);
}
// 簡中檔沒有、但人工翻譯有的句子也要補上
for (const [key, override] of Object.entries(overrides)) {
	if (!(key in translated) && key in en && override.en === en[key]) translated[key] = override.zhTW;
}

// 語言名稱固定用母語寫法，和其他語言檔一致
translated["settings.language.zhCN"] = "简体中文";
translated[ZH_TW_LABEL_KEY] = "繁體中文";

// 依英文檔的順序排列，方便日後用 git 比對差異
const catalog = {};
for (const key of Object.keys(en)) if (key in translated) catalog[key] = translated[key];
for (const key of Object.keys(translated)) if (!(key in catalog)) catalog[key] = translated[key];

// —— 品質檢查 ——
// 變數插槽（{{name}}）與標籤（<0></0>）如果遺失，畫面會少顯示資訊，必須擋下來
const tokensOf = (text) => String(text).match(/\{\{[^}]+\}\}|<\/?\d+\/?>/g) ?? [];
const lostTokens = Object.keys(en).filter((key) => {
	if (typeof en[key] !== "string" || !(key in catalog)) return false;
	const have = tokensOf(catalog[key]);
	return tokensOf(en[key]).some((token) => !have.includes(token));
});
// 英文檔有、繁中檔沒有的句子：畫面會先顯示英文
const missing = Object.keys(en).filter((key) => !(key in catalog));
// 簡中檔裡還是英文、也沒有人工翻譯的句子（通常是官方新加的）
const stillEnglish = Object.keys(en).filter(
	(key) => typeof en[key] === "string" && zhCN[key] === en[key] && /[a-z]{3}/i.test(en[key]) && !(key in overrides),
);

// 其他語言檔是否都有「繁體中文」選項名稱
const labelUpdates = [];
for (const locale of OTHER_CATALOGS) {
	const path = join(i18nDir, `${locale}.json`);
	const text = readFileSync(path, "utf8");
	if (ZH_TW_LABEL_KEY in JSON.parse(text)) continue;
	// 插在簡中名稱的下一行，保留原檔的排版
	const updated = text.replace(
		/(\n(\t+)"settings\.language\.zhCN": "[^"]*",)/,
		(line, _match, indent) => `${line}\n${indent}"${ZH_TW_LABEL_KEY}": "${zhTwLabelFor(locale)}",`,
	);
	if (updated === text) {
		console.error(`${locale}.json 找不到 settings.language.zhCN，無法自動加入繁中選項名稱，請手動加入。`);
		process.exit(1);
	}
	JSON.parse(updated);
	labelUpdates.push({ locale, path, updated });
}

// —— 報告 ——
const listSome = (keys, describe) => {
	for (const key of keys.slice(0, 15)) console.log(`    ${key}${describe ? `：${describe(key)}` : ""}`);
	if (keys.length > 15) console.log(`    …另外 ${keys.length - 15} 句`);
};
const englishOf = (key) => JSON.stringify(en[key]);

if (lostTokens.length > 0) {
	console.error(`✖ 有 ${lostTokens.length} 句翻譯遺失了變數插槽，已停止寫入：`);
	for (const key of lostTokens) console.error(`    ${key}\n      英文：${en[key]}\n      繁中：${catalog[key]}`);
	process.exit(1);
}

const output = `${JSON.stringify(catalog, null, "\t")}\n`;
let current = "";
try {
	current = readFileSync(outputPath, "utf8");
} catch {
	current = "";
}
const upToDate = current === output && labelUpdates.length === 0;

console.log(`繁中語言檔：共 ${Object.keys(catalog).length} 句`);
if (staleOverrides.length > 0) {
	console.log(`\n⚠ ${staleOverrides.length} 句人工翻譯已過時（官方改了英文），暫時改用簡中自動轉換的結果。`);
	console.log("  請到 scripts/zh-tw/overrides.json 更新這些句子的 en 與 zhTW：");
	listSome(staleOverrides, englishOf);
}
if (stillEnglish.length > 0) {
	console.log(`\n⚠ ${stillEnglish.length} 句在簡中檔裡仍是英文，繁中也會顯示英文。`);
	console.log("  若要翻譯，請在 scripts/zh-tw/overrides.json 加入 { \"en\": 英文原文, \"zhTW\": 繁中翻譯 }：");
	listSome(stillEnglish, englishOf);
}
if (missing.length > 0) {
	console.log(`\n⚠ ${missing.length} 句簡中檔還沒有，畫面會先顯示英文：`);
	listSome(missing, englishOf);
}
if (obsoleteOverrides.length > 0) {
	console.log(`\nℹ ${obsoleteOverrides.length} 句人工翻譯對應的句子已被官方移除，可以從 overrides.json 刪掉：`);
	listSome(obsoleteOverrides);
}

if (checkOnly) {
	if (upToDate) {
		console.log("\n✔ 繁中語言檔已是最新。");
		process.exit(0);
	}
	if (current !== output) console.log("\n✖ zh-TW.json 和最新產生的結果不同。");
	for (const { locale } of labelUpdates) console.log(`✖ ${locale}.json 缺少「繁體中文」選項名稱。`);
	console.log("請執行 `npm run i18n:zh-tw` 更新。");
	process.exit(1);
}

writeFileSync(outputPath, output);
for (const { locale, path, updated } of labelUpdates) {
	writeFileSync(path, updated);
	console.log(`已在 ${locale}.json 加入「繁體中文」選項名稱`);
}
console.log(upToDate ? "\n✔ 繁中語言檔原本就是最新，沒有變動。" : "\n✔ 已更新 src/renderer/i18n/zh-TW.json");

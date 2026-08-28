package modem

import (
	"strconv"
	"strings"
)

var servingOperatorNameByPLMN = map[string]string{
	// 中国大陆
	"46000": "中国移动",
	"46002": "中国移动",
	"46004": "中国移动",
	"46007": "中国移动",
	"46008": "中国移动",
	"46013": "中国移动",
	"46001": "中国联通",
	"46006": "中国联通",
	"46009": "中国联通",
	"46003": "中国电信",
	"46005": "中国电信",
	"46011": "中国电信",
	"46015": "中国广电",

	// 中国香港
	"45400": "CSL",
	"45402": "CSL",
	"45410": "CSL",
	"45418": "CSL",
	"45403": "电讯盈科",
	"45416": "电讯盈科",
	"45419": "电讯盈科",
	"45404": "3 HK",
	"45406": "数码通",
	"45415": "数码通",
	"45407": "中国移动香港",
	"45412": "中国联通香港",

	// 中国台湾
	"46601": "远传电信",
	"46602": "远传电信",
	"46605": "亚太电信",
	"46689": "台湾之星",
	"46692": "中华电信",
	"46693": "台湾大哥大",
	"46697": "台湾大哥大",
	"46699": "台湾大哥大",

	// 中国澳门
	"45500": "CTM (澳门电讯)",
	"45501": "CTM (澳门电讯)",
	"45502": "中国电信 (澳门)",
	"45503": "3 (澳门)",
	"45504": "CTM (澳门电讯)",
	"45505": "3 (澳门)",
	"45506": "数码通 (澳门)",
}

func normalizeOperatorCode(code string) string {
	code = strings.TrimSpace(code)
	return strings.Trim(code, "\"")
}

// LookupServingOperatorNameFromPLMN returns the mapped serving-network display name when the PLMN is known.
func LookupServingOperatorNameFromPLMN(plmn string) (string, bool) {
	plmn = normalizeOperatorCode(plmn)
	if plmn == "" {
		return "", false
	}
	name, ok := servingOperatorNameByPLMN[plmn]
	return name, ok
}

// ResolveServingOperatorNameFromPLMN returns a serving-network display name when known, otherwise the normalized raw PLMN.
func ResolveServingOperatorNameFromPLMN(plmn string) string {
	plmn = normalizeOperatorCode(plmn)
	if plmn == "" {
		return ""
	}
	if name, ok := LookupServingOperatorNameFromPLMN(plmn); ok {
		return name
	}
	return plmn
}

// operatorDisplayNameByCode 常见模块上报的英文运营商名/代码 → 中文显示名，
// 与网页端 app.js 的映射保持一致。
var operatorDisplayNameByCode = map[string]string{
	"CHN-UNICOM":     "中国联通",
	"CHINA UNICOM":   "中国联通",
	"UNICOM":         "中国联通",
	"CHINA MOBILE":   "中国移动",
	"CMCC":           "中国移动",
	"CHN-CMCC":       "中国移动",
	"CHINA TELECOM":  "中国电信",
	"CHN-CT":         "中国电信",
	"CTCC":           "中国电信",
	"CBN":            "中国广电",
	"CHN-CBN":        "中国广电",
	"CHINA BROADNET": "中国广电",
}

// plmnCandidatesFromIMSI 从 IMSI 提取候选 PLMN（MCC+MNC，兼容 2 位或 3 位 MNC）。
func plmnCandidatesFromIMSI(imsi string) []string {
	digits := strings.TrimSpace(imsi)
	if len(digits) < 5 {
		return nil
	}
	candidates := make([]string, 0, 2)
	if len(digits) >= 6 {
		if _, err := strconv.Atoi(digits[:6]); err == nil {
			candidates = append(candidates, digits[:6])
		}
	}
	if _, err := strconv.Atoi(digits[:5]); err == nil {
		candidates = append(candidates, digits[:5])
	}
	return candidates
}

// NormalizeServingOperatorName 把模块上报的运营商名称/代码规范化为中文显示名：
// 先按英文名/代码映射，再用 IMSI 的 PLMN 查表；仍无法识别时原样返回。
func NormalizeServingOperatorName(raw, imsi string) string {
	name := strings.TrimSpace(raw)
	key := strings.ToUpper(name)
	if display, ok := operatorDisplayNameByCode[key]; ok {
		return display
	}
	if display, found := LookupServingOperatorNameFromPLMN(name); found {
		return display
	}
	for _, plmn := range plmnCandidatesFromIMSI(imsi) {
		if display, found := LookupServingOperatorNameFromPLMN(plmn); found {
			return display
		}
	}
	return name
}

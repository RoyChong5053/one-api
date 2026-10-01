#!/usr/bin/env python3
"""Insert the channel health i18n keys into every management locale.

Kept as a script so the five locales cannot drift: the band names in particular
have to stay in step with the backend's HealthBand values.
"""
import json
import os
from collections import OrderedDict

LOCALES_DIR = os.path.join(os.path.dirname(__file__), "..", "src", "i18n", "locales")

TRANSLATIONS = {
    "en": {
        "health": {
            "band_healthy": "Healthy",
            "band_degraded": "Degraded",
            "band_unhealthy": "Unhealthy",
            "band_unknown": "Unmeasured",
            "label": "Health",
            "latency": "Latency",
            "latency_title": "Average time for a full request to complete",
            "model_count": "{{count}} models",
            "no_samples": "no samples yet",
            "rate_limit_rate": "Rate limited",
            "cut_rate": "Cut rate",
            "cut_rate_title": "Streams that ended before a final finish_reason",
            "samples_one": "{{count}} sample",
            "samples_other": "{{count}} samples",
            "success_rate": "Success",
            "tooltip_consecutive_failures": "{{count}} consecutive failure(s)",
            "tooltip_no_samples": "Not enough traffic to judge this channel yet",
            "tooltip_reasons": "Penalised for: {{reasons}}",
            "tooltip_score": "Health score {{score}}/100",
            "tooltip_suspended": "Temporarily removed from selection by the circuit breaker",
            "tps": "Tokens/s",
            "tps_title": "Average generation speed; very low values mean the upstream is throttling",
            "ttft": "First token",
            "ttft_title": "Average time until the first token arrives",
            "weight_hint": "Splits traffic between equally healthy channels. It cannot override health.",
        },
        "status": {"auto_disabled": "Auto-disabled", "unknown": "Unknown"},
        "columns": {"weight_input_label": "Weight for {{name}}"},
        "toolbar": {
            "toggle_view": "Switch between card and table layout",
            "view_cards": "Cards",
            "view_table": "Table",
        },
        "notifications": {"weight_saved": "Weight updated."},
    },
    "zh": {
        "health": {
            "band_healthy": "健康",
            "band_degraded": "降级",
            "band_unhealthy": "不健康",
            "band_unknown": "未测量",
            "label": "健康分",
            "latency": "延迟",
            "latency_title": "完整请求的平均耗时",
            "model_count": "{{count}} 个模型",
            "no_samples": "尚无样本",
            "rate_limit_rate": "限流",
            "cut_rate": "掐流率",
            "cut_rate_title": "在最终 finish_reason 之前就断掉的流",
            "samples_one": "{{count}} 个样本",
            "samples_other": "{{count}} 个样本",
            "success_rate": "成功率",
            "tooltip_consecutive_failures": "连续失败 {{count}} 次",
            "tooltip_no_samples": "样本不足，暂无法判定",
            "tooltip_reasons": "扣分原因：{{reasons}}",
            "tooltip_score": "健康分 {{score}}/100",
            "tooltip_suspended": "已被熔断器暂时移出选择范围",
            "tps": "tokens/秒",
            "tps_title": "平均生成速度；数值极低代表上游在限速",
            "ttft": "首字延迟",
            "ttft_title": "收到第一个 token 的平均时间",
            "weight_hint": "只在同样健康的渠道之间分配流量，无法凌驾于健康判定之上",
        },
        "status": {"auto_disabled": "已自动禁用", "unknown": "未知"},
        "columns": {"weight_input_label": "「{{name}}」的权重"},
        "toolbar": {
            "toggle_view": "切换卡片／表格视图",
            "view_cards": "卡片",
            "view_table": "表格",
        },
        "notifications": {"weight_saved": "权重已更新。"},
    },
    "es": {
        "health": {
            "band_healthy": "Saludable",
            "band_degraded": "Degradado",
            "band_unhealthy": "No saludable",
            "band_unknown": "Sin medir",
            "label": "Salud",
            "latency": "Latencia",
            "latency_title": "Tiempo medio de una petición completa",
            "model_count": "{{count}} modelos",
            "no_samples": "aún sin muestras",
            "rate_limit_rate": "Limitado",
            "cut_rate": "Tasa de corte",
            "cut_rate_title": "Flujos terminados antes de un finish_reason final",
            "samples_one": "{{count}} muestra",
            "samples_other": "{{count}} muestras",
            "success_rate": "Éxito",
            "tooltip_consecutive_failures": "{{count}} fallo(s) consecutivos",
            "tooltip_no_samples": "Traffic insufficient para juzgar este canal",
            "tooltip_reasons": "Penalizado por: {{reasons}}",
            "tooltip_score": "Puntuación de salud {{score}}/100",
            "tooltip_suspended": "Retirado temporalmente de la selección por el cortacircuitos",
            "tps": "Tokens/s",
            "tps_title": "Velocidad media de generación; valores muy bajos indican limitación upstream",
            "ttft": "Primer token",
            "ttft_title": "Tiempo medio hasta el primer token",
            "weight_hint": "Reparte tráfico entre canales igual de saludables. No puede anular la salud.",
        },
        "status": {"auto_disabled": "Deshabilitado automáticamente", "unknown": "Desconocido"},
        "columns": {"weight_input_label": "Peso de {{name}}"},
        "toolbar": {
            "toggle_view": "Cambiar entre vista de tarjetas y tabla",
            "view_cards": "Tarjetas",
            "view_table": "Tabla",
        },
        "notifications": {"weight_saved": "Peso actualizado."},
    },
    "fr": {
        "health": {
            "band_healthy": "Sain",
            "band_degraded": "Dégradé",
            "band_unhealthy": "Malsain",
            "band_unknown": "Non mesuré",
            "label": "Santé",
            "latency": "Latence",
            "latency_title": "Temps moyen d'une requête complète",
            "model_count": "{{count}} modèles",
            "no_samples": "pas encore d'échantillons",
            "rate_limit_rate": "Limité",
            "cut_rate": "Taux de coupure",
            "cut_rate_title": "Flux interrompus avant un finish_reason final",
            "samples_one": "{{count}} échantillon",
            "samples_other": "{{count}} échantillons",
            "success_rate": "Réussite",
            "tooltip_consecutive_failures": "{{count}} échec(s) consécutif(s)",
            "tooltip_no_samples": "Trafic insuffisant pour juger ce canal",
            "tooltip_reasons": "Pénalisé pour : {{reasons}}",
            "tooltip_score": "Score de santé {{score}}/100",
            "tooltip_suspended": "Retiré temporairement de la sélection par le coupe-circuit",
            "tps": "Tokens/s",
            "tps_title": "Vitesse de génération moyenne ; très bas = limitation amont",
            "ttft": "Premier token",
            "ttft_title": "Temps moyen avant le premier token",
            "weight_hint": "Répartit le trafic entre canaux aussi sains. Ne peut pas outrepasser la santé.",
        },
        "status": {"auto_disabled": "Désactivé automatiquement", "unknown": "Inconnu"},
        "columns": {"weight_input_label": "Poids de {{name}}"},
        "toolbar": {
            "toggle_view": "Basculer entre cartes et tableau",
            "view_cards": "Cartes",
            "view_table": "Tableau",
        },
        "notifications": {"weight_saved": "Poids mis à jour."},
    },
    "ja": {
        "health": {
            "band_healthy": "正常",
            "band_degraded": "劣化",
            "band_unhealthy": "異常",
            "band_unknown": "未計測",
            "label": "ヘルス",
            "latency": "レイテンシ",
            "latency_title": "リクエスト全体の平均応答時間",
            "model_count": "{{count}} モデル",
            "no_samples": "サンプルなし",
            "rate_limit_rate": "レート制限",
            "cut_rate": "切断率",
            "cut_rate_title": "最終 finish_reason 前に途切れたストリーム",
            "samples_one": "{{count}} サンプル",
            "samples_other": "{{count}} サンプル",
            "success_rate": "成功率",
            "tooltip_consecutive_failures": "連続失敗 {{count}} 回",
            "tooltip_no_samples": "判定にはトラフィックが不足しています",
            "tooltip_reasons": "減点理由: {{reasons}}",
            "tooltip_score": "ヘルススコア {{score}}/100",
            "tooltip_suspended": "サーキットブレーカーにより一時的に選択対象から外れています",
            "tps": "トークン/秒",
            "tps_title": "平均生成速度。極端に低い場合は上流が制限しています",
            "ttft": "最初のトークン",
            "ttft_title": "最初のトークンまでの平均時間",
            "weight_hint": "同等に正常なチャネル間でトラフィックを分配します。ヘルスを上書きはできません。",
        },
        "status": {"auto_disabled": "自動無効", "unknown": "不明"},
        "columns": {"weight_input_label": "{{name}} の重み"},
        "toolbar": {
            "toggle_view": "カード／テーブル表示を切り替え",
            "view_cards": "カード",
            "view_table": "テーブル",
        },
        "notifications": {"weight_saved": "重みを更新しました。"},
    },
}


def merge(existing, incoming):
    """Recursively merge incoming into existing without reordering existing keys."""
    for key, value in incoming.items():
        if isinstance(value, dict):
            node = existing.get(key)
            if not isinstance(node, dict):
                node = OrderedDict()
                existing[key] = node
            merge(node, value)
        else:
            existing[key] = value
    return existing


def main():
    for locale, payload in TRANSLATIONS.items():
        path = os.path.join(LOCALES_DIR, locale, "management.json")
        with open(path, encoding="utf-8") as handle:
            data = json.load(handle, object_pairs_hook=OrderedDict)

        channels = data.setdefault("channels", OrderedDict())
        merge(channels, payload)

        with open(path, "w", encoding="utf-8") as handle:
            json.dump(data, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
        print(f"updated {locale}")


if __name__ == "__main__":
    main()
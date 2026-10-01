import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import { Activity, AlertTriangle, Ban, HelpCircle } from 'lucide-react';
import { useTranslation } from 'react-i18next';

/**
 * The health bands mirror the backend's HealthBand values in
 * model/channel_health.go. They must stay in step: the band shown here is the
 * same band channel selection gates on, so a mismatch would mean the UI
 * misrepresents what the router is actually doing.
 */
export type HealthBand = 'healthy' | 'degraded' | 'unhealthy' | 'unknown';

export interface ChannelHealth {
  channel_id: number;
  score: number;
  band: HealthBand;
  success_rate: number;
  latency_ms: number;
  ttft_ms: number;
  tps: number;
  cut_rate: number;
  rate_limit_rate: number;
  consecutive_failures: number;
  consecutive_probes: number;
  samples: number;
  suspended: boolean;
  suspend_until: number;
  updated_time: number;
  reasons: string[];
}

const bandStyles: Record<HealthBand, { badge: string; bar: string; labelKey: string }> = {
  healthy: {
    badge: 'bg-success-muted text-success-foreground border-success-border',
    bar: 'bg-success',
    labelKey: 'channels.health.band_healthy',
  },
  degraded: {
    badge: 'bg-warning-muted text-warning-foreground border-warning-border',
    bar: 'bg-warning',
    labelKey: 'channels.health.band_degraded',
  },
  unhealthy: {
    badge: 'bg-destructive/10 text-destructive border-destructive/30',
    bar: 'bg-destructive',
    labelKey: 'channels.health.band_unhealthy',
  },
  unknown: {
    badge: 'bg-muted text-muted-foreground',
    bar: 'bg-muted-foreground/40',
    labelKey: 'channels.health.band_unknown',
  },
};

const bandIcons: Record<HealthBand, typeof Activity> = {
  healthy: Activity,
  degraded: AlertTriangle,
  unhealthy: Ban,
  unknown: HelpCircle,
};

/**
 * HealthBadge renders the composite health score together with the band it
 * falls in. A bare number would be hard to act on, so the tooltip spells out
 * what is costing the channel points.
 */
export function HealthBadge({ health }: { health?: ChannelHealth | null }) {
  const { t } = useTranslation();

  if (!health) {
    return (
      <Badge variant="outline" className="gap-1 text-xs text-muted-foreground" data-health-band="unknown">
        <HelpCircle className="h-3 w-3" />
        {t('channels.health.band_unknown')}
      </Badge>
    );
  }

  const style = bandStyles[health.band] ?? bandStyles.unknown;
  const Icon = bandIcons[health.band] ?? HelpCircle;
  const percent = Math.round(Math.max(0, Math.min(1, health.score)) * 100);

  const titleParts = [t('channels.health.tooltip_score', { score: percent })];
  if (health.samples <= 0) {
    titleParts.push(t('channels.health.tooltip_no_samples'));
  } else {
    if (health.reasons.length > 0) {
      titleParts.push(t('channels.health.tooltip_reasons', { reasons: health.reasons.join(', ') }));
    }
    if (health.consecutive_failures > 0) {
      titleParts.push(t('channels.health.tooltip_consecutive_failures', { count: health.consecutive_failures }));
    }
    if (health.suspended) {
      titleParts.push(t('channels.health.tooltip_suspended'));
    }
  }

  return (
    <Badge
      variant="outline"
      className={cn('gap-1.5 text-xs', style.badge)}
      title={titleParts.join('\n')}
      // Exposed so the rendered state can be asserted without depending on
      // whichever language the UI is currently in.
      data-health-band={health.band}
      data-health-score={percent}
    >
      <Icon className="h-3 w-3 flex-shrink-0" />
      <span className="font-mono tabular-nums">{percent}</span>
      <span>{t(style.labelKey)}</span>
    </Badge>
  );
}

/**
 * HealthBar is the compact score bar used inside the card body. It carries no
 * label of its own; the surrounding layout supplies one.
 */
export function HealthBar({ health }: { health?: ChannelHealth | null }) {
  if (!health || health.samples <= 0) {
    return <div className="h-1.5 w-full rounded-full bg-muted" />;
  }

  const style = bandStyles[health.band] ?? bandStyles.unknown;
  const percent = Math.max(0, Math.min(1, health.score));

  return (
    <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
      <div
        className={cn('h-full rounded-full transition-all duration-300', style.bar)}
        style={{ width: `${percent * 100}%` }}
      />
    </div>
  );
}
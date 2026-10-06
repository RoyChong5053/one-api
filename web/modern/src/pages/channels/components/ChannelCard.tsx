import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { ListActionButton } from '@/components/ui/list-action-button';
import { TimestampDisplay } from '@/components/ui/timestamp';
import { cn } from '@/lib/utils';
import { Ban, CheckCircle, Copy, FlaskConical, Gauge, Settings, Star, Trash2, Zap } from 'lucide-react';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { type ChannelHealth, HealthBadge, HealthBar } from './ChannelHealthBadge';

export interface ChannelCardData {
  id: number;
  name: string;
  type: number;
  status: number;
  favorite?: boolean;
  priority?: number;
  weight?: number;
  balance?: number;
  balance_updated_time?: number;
  response_time?: number;
  test_time?: number;
  models?: string;
  group?: string;
  health?: ChannelHealth | null;
}

interface ChannelCardProps {
  channel: ChannelCardData;
  typeLabel: string;
  typeColor?: string;
  refreshingBalance: boolean;
  onEdit: () => void;
  onDuplicate: () => void;
  onToggleStatus: () => void;
  onTest: () => void;
  onDelete: () => void;
  onRefreshBalance: () => void;
  onToggleFavorite: () => void;
  onPriorityChange: (value: number, field: 'priority' | 'weight') => void;
}

const CHANNEL_STATUS_ENABLED = 1;
const CHANNEL_STATUS_MANUALLY_DISABLED = 2;
const CHANNEL_STATUS_AUTO_DISABLED = 3;

/**
 * StatusBadge distinguishes the disabled states. Previously only status === 2
 * was checked, which meant an auto-disabled channel rendered as a green
 * "Active" badge whenever its priority was non-negative — the exact opposite
 * of what an operator reading the list would conclude.
 */
function StatusBadge({ status, priority }: { status: number; priority?: number }) {
  const { t } = useTranslation();

  if (status === CHANNEL_STATUS_MANUALLY_DISABLED) {
    return <Badge variant="destructive">{t('channels.status.disabled')}</Badge>;
  }
  if (status === CHANNEL_STATUS_AUTO_DISABLED) {
    return (
      <Badge variant="outline" className="border-warning-border bg-warning-muted text-warning-foreground">
        {t('channels.status.auto_disabled')}
      </Badge>
    );
  }
  if (status !== CHANNEL_STATUS_ENABLED) {
    return <Badge variant="outline">{t('channels.status.unknown')}</Badge>;
  }
  if ((priority ?? 0) < 0) {
    return (
      <Badge variant="secondary" className="bg-warning-muted text-warning-foreground">
        {t('channels.status.paused')}
      </Badge>
    );
  }
  return (
    <Badge variant="default" className="bg-success-muted text-success-foreground">
      {t('channels.status.active')}
    </Badge>
  );
}

/** Metric applies the same absolute latency thresholds the backend scores on. */
function Metric({
  icon,
  label,
  value,
  tone = 'default',
  title,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  tone?: 'default' | 'success' | 'warning' | 'destructive';
  title?: string;
}) {
  const toneClass =
    tone === 'success'
      ? 'text-success'
      : tone === 'warning'
        ? 'text-warning'
        : tone === 'destructive'
          ? 'text-destructive'
          : 'text-foreground';

  return (
    <div className="min-w-0" title={title}>
      <div className="flex items-center gap-1 text-muted-foreground">
        {icon}
        <span className="truncate text-[11px] font-medium uppercase tracking-wide">{label}</span>
      </div>
      <div className={cn('mt-0.5 font-mono text-sm tabular-nums', toneClass)}>{value}</div>
    </div>
  );
}

function formatMs(ms?: number): { text: string; tone: 'default' | 'success' | 'warning' | 'destructive' } {
  if (!ms || ms <= 0) return { text: '-', tone: 'default' };
  if (ms < 1000) return { text: `${Math.round(ms)}ms`, tone: 'success' };
  if (ms < 5000) return { text: `${Math.round(ms)}ms`, tone: 'warning' };
  return { text: `${Math.round(ms)}ms`, tone: 'destructive' };
}

function formatTps(tps?: number): { text: string; tone: 'default' | 'success' | 'warning' | 'destructive' } {
  if (!tps || tps <= 0) return { text: '-', tone: 'default' };
  if (tps >= 20) return { text: `${tps.toFixed(1)}`, tone: 'success' };
  if (tps >= 5) return { text: `${tps.toFixed(1)}`, tone: 'warning' };
  return { text: `${tps.toFixed(1)}`, tone: 'destructive' };
}

function formatPercent(rate?: number): { text: string; tone: 'default' | 'success' | 'warning' | 'destructive' } {
  if (rate === undefined || rate === null) return { text: '-', tone: 'default' };
  const pct = Math.round(rate * 100);
  if (pct <= 0) return { text: '0%', tone: 'success' };
  if (pct < 20) return { text: `${pct}%`, tone: 'warning' };
  return { text: `${pct}%`, tone: 'destructive' };
}

/**
 * ChannelCard renders one channel per card.
 *
 * The card view exists because the table needs twelve columns to stay legible
 * and therefore forces horizontal scrolling on anything narrower than a large
 * monitor. Cards wrap instead, so every field stays reachable without zooming
 * out, and they carry the health score the router is gating on.
 */
export function ChannelCard({
  channel,
  typeLabel,
  typeColor,
  refreshingBalance,
  onEdit,
  onDuplicate,
  onToggleStatus,
  onTest,
  onDelete,
  onRefreshBalance,
  onToggleFavorite,
  onPriorityChange,
}: ChannelCardProps) {
  const { t } = useTranslation();
  const health = channel.health;

  const latency = formatMs(health?.latency_ms || channel.response_time);
  const ttft = formatMs(health?.ttft_ms);
  const tps = formatTps(health?.tps);
  const cutRate = formatPercent(health?.cut_rate);
  const rateLimitRate = formatPercent(health?.rate_limit_rate);

  const modelCount = (channel.models || '').split(',').filter((m) => m.trim()).length;
  const isEnabled = channel.status === CHANNEL_STATUS_ENABLED;

  const [priorityDraft, setPriorityDraft] = useState(String(channel.priority ?? 0));
  useEffect(() => {
    setPriorityDraft(String(channel.priority ?? 0));
  }, [channel.priority]);

  const commitPriority = () => {
    const parsed = parseInt(priorityDraft.trim(), 10);
    if (!Number.isFinite(parsed)) {
      setPriorityDraft(String(channel.priority ?? 0));
      return;
    }
    if (parsed === (channel.priority ?? 0)) return;
    onPriorityChange(parsed, 'priority');
  };

  const [weightDraft, setWeightDraft] = useState(String(channel.weight ?? 0));
  useEffect(() => {
    setWeightDraft(String(channel.weight ?? 0));
  }, [channel.weight]);

  const commitWeight = () => {
    const parsed = parseInt(weightDraft.trim(), 10);
    if (!Number.isFinite(parsed) || parsed < 0) {
      setWeightDraft(String(channel.weight ?? 0));
      return;
    }
    if (parsed === (channel.weight ?? 0)) return;
    // Reuses the priority update path: both are single-field channel updates.
    onPriorityChange(parsed, 'weight');
  };

  return (
    <div
      data-testid="data-row"
      data-channel-id={channel.id}
      className={cn(
        'flex flex-col gap-3 rounded-lg border bg-card p-4 shadow-sm transition-colors',
        'hover:bg-muted/40 focus-within:border-primary/50',
        channel.status === CHANNEL_STATUS_AUTO_DISABLED && 'border-warning-border/60'
      )}
    >
      {/* Header: identity and verdict */}
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            {typeColor && (
              <span className="inline-block h-2.5 w-2.5 flex-shrink-0 rounded-full" style={{ backgroundColor: typeColor }} />
            )}
            <button
              type="button"
              onClick={onToggleFavorite}
              aria-label={t('channels.actions.favorite', 'Pin channel')}
              title={channel.favorite ? t('channels.actions.unfavorite', 'Unpin channel') : t('channels.actions.favorite', 'Pin channel')}
              className="flex-shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:text-yellow-500"
            >
              <Star className={cn('h-3.5 w-3.5', channel.favorite && 'fill-current text-yellow-500')} />
            </button>
            <span className="truncate font-medium" title={channel.name}>
              {channel.name}
            </span>
          </div>
          <div className="mt-1 flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
            <span className="truncate">{typeLabel}</span>
            {channel.group && (
              <>
                <span aria-hidden="true">·</span>
                <span className="truncate">{channel.group}</span>
              </>
            )}
            <span aria-hidden="true">·</span>
            <span className="font-mono">#{channel.id}</span>
          </div>
        </div>
        <div className="flex flex-shrink-0 flex-col items-end gap-1.5">
          <HealthBadge health={health} />
          <StatusBadge status={channel.status} priority={channel.priority} />
        </div>
      </div>

      {/* Health score bar */}
      <div>
        <div className="mb-1 flex items-baseline justify-between text-xs">
          <span className="font-medium uppercase tracking-wide text-muted-foreground">{t('channels.health.label')}</span>
          <span className="font-mono tabular-nums text-muted-foreground">
            {health?.samples ? t('channels.health.samples', { count: health.samples }) : t('channels.health.no_samples')}
          </span>
        </div>
        <HealthBar health={health} />
        {health && health.reasons.length > 0 && (
          <p className="mt-1.5 line-clamp-2 text-xs text-muted-foreground" title={health.reasons.join(', ')}>
            {health.reasons.join(' · ')}
          </p>
        )}
      </div>

      {/* Measured signals */}
      <div className="grid grid-cols-2 gap-x-3 gap-y-2.5 sm:grid-cols-3">
        <Metric
          icon={<Gauge className="h-3 w-3" />}
          label={t('channels.health.latency')}
          value={latency.text}
          tone={latency.tone}
          title={t('channels.health.latency_title')}
        />
        <Metric
          icon={<Zap className="h-3 w-3" />}
          label={t('channels.health.ttft')}
          value={ttft.text}
          tone={ttft.tone}
          title={t('channels.health.ttft_title')}
        />
        <Metric
          icon={<Zap className="h-3 w-3" />}
          label={t('channels.health.tps')}
          value={tps.text}
          tone={tps.tone}
          title={t('channels.health.tps_title')}
        />
        <Metric
          icon={<Gauge className="h-3 w-3" />}
          label={t('channels.health.success_rate')}
          value={formatPercent(health?.success_rate).text}
          tone={formatPercent(health?.success_rate).tone}
        />
        <Metric
          icon={<Ban className="h-3 w-3" />}
          label={t('channels.health.cut_rate')}
          value={cutRate.text}
          tone={cutRate.tone}
          title={t('channels.health.cut_rate_title')}
        />
        <Metric
          icon={<Gauge className="h-3 w-3" />}
          label={t('channels.health.rate_limit_rate')}
          value={rateLimitRate.text}
          tone={rateLimitRate.tone}
        />
      </div>

      {/* Routing configuration */}
      <div className="grid grid-cols-2 gap-3 border-t pt-3">
        <div>
          <label className="mb-1 block text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            {t('channels.columns.priority')}
          </label>
          <Input
            type="number"
            value={priorityDraft}
            aria-label={t('channels.columns.priority_input_label', { name: channel.name })}
            className="h-8 font-mono text-sm"
            onChange={(e) => setPriorityDraft(e.target.value)}
            onBlur={commitPriority}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault();
                (e.target as HTMLInputElement).blur();
              }
            }}
          />
        </div>
        <div>
          <label className="mb-1 block text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            {t('channels.columns.weight')}
          </label>
          <Input
            type="number"
            min={0}
            value={weightDraft}
            aria-label={t('channels.columns.weight_input_label', { name: channel.name })}
            className="h-8 font-mono text-sm"
            onChange={(e) => setWeightDraft(e.target.value)}
            onBlur={commitWeight}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault();
                (e.target as HTMLInputElement).blur();
              }
            }}
          />
          <p className="mt-1 text-[11px] leading-tight text-muted-foreground">{t('channels.health.weight_hint')}</p>
        </div>
      </div>

      {/* Balance and models */}
      <div className="flex items-center justify-between gap-2 text-sm">
        <div className="min-w-0">
          <span className="text-muted-foreground">{t('channels.columns.balance')} </span>
          <span className="font-mono">{typeof channel.balance === 'number' ? channel.balance.toFixed(2) : '-'}</span>
          {channel.balance_updated_time ? (
            <div className="text-xs text-muted-foreground">
              <TimestampDisplay timestamp={channel.balance_updated_time} className="font-mono" />
            </div>
          ) : null}
        </div>
        <div className="flex items-center gap-2">
          <Badge variant="outline" className="text-xs">
            {t('channels.health.model_count', { count: modelCount })}
          </Badge>
          <ListActionButton
            onClick={onRefreshBalance}
            title={t('channels.actions.refresh_balance', { name: channel.name })}
            aria-label={t('channels.actions.refresh_balance', { name: channel.name })}
            icon={<Gauge className="h-3.5 w-3.5" />}
            disabled={refreshingBalance}
          />
        </div>
      </div>

      {channel.test_time ? (
        <div className="text-xs text-muted-foreground">
          {t('channels.response.prefix')} <TimestampDisplay timestamp={channel.test_time} className="font-mono" />
        </div>
      ) : null}

      {/* Actions */}
      <div className="flex flex-wrap items-center gap-1.5 border-t pt-3">
        <Button variant="outline" size="sm" className="h-8 gap-1 px-2 text-xs" onClick={onEdit}>
          <Settings className="h-3 w-3" />
          {t('channels.actions.edit')}
        </Button>
        <Button variant="outline" size="sm" className="h-8 gap-1 px-2 text-xs" onClick={onDuplicate}>
          <Copy className="h-3 w-3" />
          {t('channels.actions.duplicate', 'Duplicate')}
        </Button>
        <Button
          variant="outline"
          size="sm"
          className={cn('h-8 gap-1 px-2 text-xs', isEnabled ? 'text-warning hover:text-warning/80' : 'text-success hover:text-success/80')}
          onClick={onToggleStatus}
        >
          {isEnabled ? <Ban className="h-3 w-3" /> : <CheckCircle className="h-3 w-3" />}
          {isEnabled ? t('channels.actions.disable') : t('channels.actions.enable')}
        </Button>
        <Button variant="outline" size="sm" className="h-8 gap-1 px-2 text-xs" onClick={onTest}>
          <FlaskConical className="h-3 w-3" />
          {t('channels.actions.test')}
        </Button>
        <Button variant="destructive" size="sm" className="ml-auto h-8 gap-1 px-2 text-xs" onClick={onDelete}>
          <Trash2 className="h-3 w-3" />
          {t('channels.actions.delete')}
        </Button>
      </div>
    </div>
  );
}
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { BrowserRouter } from 'react-router-dom';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { ChannelsPage } from '../ChannelsPage';
import { api } from '@/lib/api';

vi.mock('@/components/ui/notifications', () => ({
  useNotifications: () => ({ notify: vi.fn() }),
}));

vi.mock('@/lib/api', () => ({
  api: { get: vi.fn(), delete: vi.fn(), post: vi.fn(), put: vi.fn() },
}));

// Render at desktop width so the card/table distinction is exercised on a
// non-mobile viewport, which is where the overflow problem the card view
// solves actually appears.
vi.mock('@/hooks/useResponsive', () => ({
  useResponsive: () => ({ isMobile: false, isTablet: false }),
}));

vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual('react-router-dom');
  return { ...actual, useNavigate: () => vi.fn() };
});

const mockApiGet = vi.mocked(api.get);
const mockApiPut = vi.mocked(api.put);

const healthyChannel = {
  id: 1,
  name: 'NVIDIA-FAST',
  type: 50,
  status: 1,
  priority: 0,
  weight: 1,
  models: 'a,b,c',
  balance: 5.5,
  balance_updated_time: 1700000000,
  health: {
    channel_id: 1,
    score: 0.92,
    band: 'healthy',
    success_rate: 1,
    latency_ms: 900,
    ttft_ms: 240,
    tps: 42.5,
    cut_rate: 0,
    rate_limit_rate: 0,
    consecutive_failures: 0,
    consecutive_probes: 4,
    samples: 12,
    suspended: false,
    suspend_until: 0,
    updated_time: 1700000000,
    reasons: [],
  },
};

const unhealthyChannel = {
  id: 2,
  name: 'NVIDIA-STALLED',
  type: 50,
  // 3 is auto-disabled; it must not render as an active channel.
  status: 3,
  priority: 0,
  weight: 100,
  models: 'a,b',
  health: {
    channel_id: 2,
    score: 0.05,
    band: 'unhealthy',
    success_rate: 0.1,
    latency_ms: 12000,
    ttft_ms: 4000,
    tps: 1.1,
    cut_rate: 0.6,
    rate_limit_rate: 0.2,
    consecutive_failures: 5,
    consecutive_probes: 0,
    samples: 30,
    suspended: true,
    suspend_until: 1700003600,
    updated_time: 1700000000,
    reasons: ['slow responses', 'low throughput', 'streams cut mid-response'],
  },
};

const renderPage = () =>
  render(
    <BrowserRouter>
      <ChannelsPage />
    </BrowserRouter>
  );

describe('ChannelsPage card layout', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
    window.history.replaceState(null, '', '/');
    mockApiGet.mockResolvedValue({ data: { success: true, data: [healthyChannel, unhealthyChannel], total: 2 } });
    mockApiPut.mockResolvedValue({ data: { success: true } });
  });

  it('defaults to the card layout and renders one card per channel', async () => {
    renderPage();

    const rows = await waitFor(() => {
      const found = document.querySelectorAll('[data-testid="data-row"]');
      expect(found).toHaveLength(2);
      return found;
    });

    // Cards, not table rows.
    expect(document.querySelectorAll('tbody tr')).toHaveLength(0);
    expect(rows[0].tagName).toBe('DIV');
    expect(within(rows[0] as HTMLElement).getByText('NVIDIA-FAST')).toBeInTheDocument();
    expect(within(rows[1] as HTMLElement).getByText('NVIDIA-STALLED')).toBeInTheDocument();
  });

  it('renders unix-second timestamps without a double millisecond conversion', async () => {
    renderPage();

    const rows = await waitFor(() => {
      const found = document.querySelectorAll('[data-testid="data-row"]');
      expect(found).toHaveLength(2);
      return found;
    });

    // balance_updated_time is unix seconds (1700000000 -> Nov 2023). Passing
    // milliseconds to TimestampDisplay would render year 55873 instead.
    const card = within(rows[0] as HTMLElement);
    expect(card.getByText(/2023/)).toBeInTheDocument();
    expect(rows[0].textContent).not.toMatch(/558\d\d/);
  });

  it('surfaces the health score and band the router is gating on', async () => {
    renderPage();

    const healthyRow = await waitFor(() => {
      const rows = document.querySelectorAll('[data-testid="data-row"]');
      expect(rows).toHaveLength(2);
      return rows[0] as HTMLElement;
    });

    // Asserted via data attributes so the test does not depend on the active UI
    // language.
    const badge = healthyRow.querySelector('[data-health-band]');
    expect(badge).not.toBeNull();
    expect(badge?.getAttribute('data-health-band')).toBe('healthy');
    expect(badge?.getAttribute('data-health-score')).toBe('92');
  });

  it('shows the measured signals that make up the score', async () => {
    renderPage();

    const row = await waitFor(() => {
      const rows = document.querySelectorAll('[data-testid="data-row"]');
      expect(rows).toHaveLength(2);
      return rows[0] as HTMLElement;
    });

    expect(within(row).getByText('900ms')).toBeInTheDocument(); // latency
    expect(within(row).getByText('240ms')).toBeInTheDocument(); // first token
    expect(within(row).getByText('42.5')).toBeInTheDocument(); // tokens/sec
  });

  it('lists why a channel lost points', async () => {
    renderPage();

    const row = await waitFor(() => {
      const rows = document.querySelectorAll('[data-testid="data-row"]');
      expect(rows).toHaveLength(2);
      return rows[1] as HTMLElement;
    });

    const reasons = within(row).getByText(/low throughput/);
    expect(reasons).toBeInTheDocument();
    expect(reasons.getAttribute('title')).toContain('streams cut mid-response');
  });

  it('does not present an auto-disabled channel as active', async () => {
    renderPage();

    const row = await waitFor(() => {
      const rows = document.querySelectorAll('[data-testid="data-row"]');
      expect(rows).toHaveLength(2);
      return rows[1] as HTMLElement;
    });

    // The status badge must reflect status 3 (auto-disabled), not green Active.
    // Asserted structurally: the active badge is the only one carrying the
    // success-muted background.
    expect(row.querySelector('.bg-success-muted')).toBeNull();
    expect(within(row).getByText('Auto-disabled')).toBeInTheDocument();
  });

  it('offers the row actions inside the card itself', async () => {
    renderPage();

    const row = await waitFor(() => {
      const rows = document.querySelectorAll('[data-testid="data-row"]');
      expect(rows).toHaveLength(2);
      return rows[0] as HTMLElement;
    });

    // Floating hover actions do not exist on a card, so every action has to be
    // present in the card body.
    for (const label of ['Edit', 'Test', 'Delete']) {
      expect(within(row).getByRole('button', { name: label })).toBeInTheDocument();
    }
  });

  it('persists weight edits through the single-field update path', async () => {
    const user = userEvent.setup();
    renderPage();

    const row = await waitFor(() => {
      const rows = document.querySelectorAll('[data-testid="data-row"]');
      expect(rows).toHaveLength(2);
      return rows[0] as HTMLElement;
    });

    const weightInput = within(row).getByRole('spinbutton', { name: /Weight for/ });
    await user.clear(weightInput);
    await user.type(weightInput, '5');
    await user.tab();

    await waitFor(() => {
      expect(mockApiPut).toHaveBeenCalledWith('/api/channel/', {
        id: 1,
        name: 'NVIDIA-FAST',
        weight: 5,
      });
    });
  });

  it('switches to the table layout and back, remembering the choice', async () => {
    const user = userEvent.setup();
    renderPage();

    await waitFor(() => {
      expect(document.querySelectorAll('[data-testid="data-row"]')).toHaveLength(2);
    });

    await user.click(screen.getByRole('button', { name: /Table/ }));

    await waitFor(() => {
      expect(document.querySelectorAll('tbody tr')).toHaveLength(2);
    });
    expect(document.querySelectorAll('[data-testid="data-row"]')[0].tagName).toBe('TR');
    expect(localStorage.getItem('channels.view_mode')).toBe('table');

    await user.click(screen.getByRole('button', { name: /Cards/ }));

    await waitFor(() => {
      expect(document.querySelectorAll('tbody tr')).toHaveLength(0);
      expect(document.querySelectorAll('[data-testid="data-row"]')).toHaveLength(2);
    });
    expect(localStorage.getItem('channels.view_mode')).toBe('cards');
  });

  it('honours a stored table preference on mount', async () => {
    localStorage.setItem('channels.view_mode', 'table');
    renderPage();

    await waitFor(() => {
      expect(document.querySelectorAll('tbody tr')).toHaveLength(2);
    });
  });
});
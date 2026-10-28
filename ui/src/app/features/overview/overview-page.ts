import { CommonModule } from '@angular/common';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { AdminApiService } from '../../core/admin-api.service';
import { AuthService } from '../../core/auth.service';
import { FeedbackService } from '../../core/feedback.service';
import { ActiveDownload, PreloadJob, Repository, Stats } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-overview-page',
  standalone: true,
  imports: [CommonModule, IconComponent, PageHeadingComponent, RouterLink],
  templateUrl: './overview-page.html',
})
export class OverviewPageComponent implements OnInit, OnDestroy {
  private readonly api = inject(AdminApiService);
  protected readonly auth = inject(AuthService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly stats = signal<Stats | null>(null);
  protected readonly repositories = signal<Repository[]>([]);
  protected readonly activeDownloads = signal<ActiveDownload[]>([]);
  protected readonly preloadJobs = signal<PreloadJob[]>([]);
  private transferPoll?: number;
  private transferPollRunning = false;

  ngOnInit(): void {
    void this.load();
    void this.loadTransfers();
    this.transferPoll = window.setInterval(() => void this.loadTransfers(), 2000);
  }

  ngOnDestroy(): void {
    if (this.transferPoll !== undefined) window.clearInterval(this.transferPoll);
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      const [stats, repositories] = await Promise.all([
        this.api.get<Stats>('/api/v1/stats'),
        this.api.listAll<Repository>('/api/v1/repos'),
      ]);
      this.stats.set(stats);
      this.repositories.set(repositories);
      this.feedback.clear();
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to load your overview.'));
    } finally {
      this.loading.set(false);
    }
  }

  protected get hitPercent(): number {
    return Math.round((this.stats()?.hit_ratio ?? 0) * 100);
  }

  protected get hitCount(): string {
    return this.compact(this.stats()?.cache_hits ?? 0);
  }

  protected get missCount(): string {
    return this.compact(this.stats()?.cache_misses ?? 0);
  }

  protected get canManageRepositories(): boolean {
    return this.auth.principal()?.role === 'admin';
  }

  protected typeLabel(type: string): string {
    return type === 'datasets' ? 'Dataset' : type === 'spaces' ? 'Space' : 'Model';
  }

  protected policyLabel(policy: string): string {
    return policy === 'admin_only'
      ? 'Admins only'
      : policy === 'allowlist'
        ? 'Allowlist'
        : 'Public';
  }

  protected bytes(value: number): string {
    if (value < 1024) return `${value} B`;
    const units = ['KiB', 'MiB', 'GiB', 'TiB'];
    let size = value;
    for (const unit of units) {
      size /= 1024;
      if (size < 1024 || unit === 'TiB') return `${size.toFixed(1)} ${unit}`;
    }
    return `${value} B`;
  }

  protected preloadPercent(job: PreloadJob): number {
    return job.files_total === 0 ? 0 : Math.round((job.files_processed / job.files_total) * 100);
  }

  protected preloadStatus(status: PreloadJob['status']): string {
    if (status === 'completed_with_errors') return 'Completed with errors';
    if (status === 'incomplete') return 'Cache incomplete';
    return status.charAt(0).toUpperCase() + status.slice(1);
  }

  private compact(value: number): string {
    return new Intl.NumberFormat('en', { notation: 'compact', maximumFractionDigits: 1 }).format(
      value,
    );
  }

  private async loadTransfers(): Promise<void> {
    if (this.transferPollRunning) return;
    this.transferPollRunning = true;
    try {
      const result = await this.api.get<{ items: ActiveDownload[]; preloads: PreloadJob[] }>(
        '/api/v1/downloads/active',
      );
      this.activeDownloads.set(result.items);
      this.preloadJobs.set(result.preloads);
    } catch {
      // Keep the most recent transfer state visible if a poll briefly fails.
      if (!this.auth.principal() && this.transferPoll !== undefined) {
        window.clearInterval(this.transferPoll);
        this.transferPoll = undefined;
      }
    } finally {
      this.transferPollRunning = false;
    }
  }
}

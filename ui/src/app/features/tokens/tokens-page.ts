import { CommonModule } from '@angular/common';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApiService } from '../../core/admin-api.service';
import { FeedbackService } from '../../core/feedback.service';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

interface AccessToken {
  id: string;
  name: string;
  prefix: string;
  scopes: string[];
  created_at: string;
  expires_at: string | null;
  last_used_at: string | null;
}

@Component({
  selector: 'app-tokens-page',
  standalone: true,
  imports: [CommonModule, FormsModule, PageHeadingComponent],
  templateUrl: './tokens-page.html',
})
export class TokensPageComponent implements OnInit, OnDestroy {
  private readonly api = inject(AdminApiService);
  private readonly feedback = inject(FeedbackService);
  private destroyed = false;
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly tokens = signal<AccessToken[]>([]);
  protected readonly secret = signal('');
  protected readonly confirming = signal('');
  protected draft = { name: '', expires_in_days: 90 };

  ngOnInit(): void {
    void this.load();
  }

  ngOnDestroy(): void {
    this.destroyed = true;
    this.secret.set('');
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      const result = await this.api.get<{ items: AccessToken[] }>('/api/v1/me/tokens');
      if (!this.destroyed) this.tokens.set(result.items);
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to load your tokens.'));
    } finally {
      this.loading.set(false);
    }
  }

  protected async create(): Promise<void> {
    const name = this.draft.name.trim();
    const days = this.draft.expires_in_days;
    if (
      this.busy() ||
      this.secret() ||
      !name ||
      name.length > 100 ||
      !Number.isInteger(days) ||
      days < 1 ||
      days > 365
    )
      return;
    this.busy.set(true);
    try {
      const result = await this.api.post<AccessToken & { token: string }>('/api/v1/me/tokens', {
        name,
        expires_in_days: days,
      });
      if (this.destroyed) return;
      this.secret.set(result.token);
      this.draft.name = '';
      await this.load();
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to create a token.'));
    } finally {
      this.busy.set(false);
    }
  }

  protected async revoke(token: AccessToken): Promise<void> {
    if (this.busy()) return;
    this.busy.set(true);
    try {
      await this.api.delete(`/api/v1/me/tokens/${encodeURIComponent(token.id)}`);
      this.tokens.update((items) => items.filter((item) => item.id !== token.id));
      this.confirming.set('');
      this.feedback.showSuccess('Token revoked. It can no longer access hfproxy.');
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to revoke this token.'));
    } finally {
      this.busy.set(false);
    }
  }

  protected expired(token: AccessToken): boolean {
    return token.expires_at !== null && Date.parse(token.expires_at) <= Date.now();
  }
}

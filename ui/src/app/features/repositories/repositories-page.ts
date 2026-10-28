import { CommonModule } from '@angular/common';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AdminApiService } from '../../core/admin-api.service';
import { AuthService } from '../../core/auth.service';
import { FeedbackService } from '../../core/feedback.service';
import { Grant, Policy, PreloadJob, RepoType, Repository } from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

@Component({
  selector: 'app-repositories-page',
  standalone: true,
  imports: [CommonModule, FormsModule, IconComponent, PageHeadingComponent],
  templateUrl: './repositories-page.html',
})
export class RepositoriesPageComponent implements OnInit, OnDestroy {
  private readonly api = inject(AdminApiService);
  private readonly auth = inject(AuthService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly repositories = signal<Repository[]>([]);
  private readonly savedPolicies = new Map<string, Policy>();
  protected readonly preloading = signal<Record<string, boolean>>({});
  protected readonly preloadJobs = signal<PreloadJob[]>([]);
  protected readonly selectedRepo = signal<Repository | null>(null);
  protected readonly grants = signal<Grant[]>([]);
  private preloadPoll?: number;
  private preloadPollRunning = false;

  protected repoDraft: { repo_type: RepoType; repo_id: string; access_policy: Policy } = {
    repo_type: 'models',
    repo_id: '',
    access_policy: 'admin_only',
  };
  protected grantDraft = { subject_type: 'user', subject_id: '', permission: 'read' };

  ngOnInit(): void {
    void this.load();
    void this.loadPreloads();
    this.preloadPoll = window.setInterval(() => void this.loadPreloads(), 2000);
  }

  ngOnDestroy(): void {
    if (this.preloadPoll !== undefined) window.clearInterval(this.preloadPoll);
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      const repositories = await this.api.listAll<Repository>('/api/v1/repos');
      this.repositories.set(repositories);
      this.savedPolicies.clear();
      for (const repository of repositories) this.savedPolicies.set(repository.id, repository.access_policy);
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, 'Unable to load repository policies.'),
      );
    } finally {
      this.loading.set(false);
    }
  }

  protected async createRepository(): Promise<void> {
    const repoID = this.repoDraft.repo_id.trim();
    if (!repoID) return;
    await this.mutate(async () => {
      await this.api.post('/api/v1/repos', { ...this.repoDraft, repo_id: repoID });
      this.repoDraft.repo_id = '';
      await this.load();
      this.feedback.showSuccess('Repository access policy saved.');
    });
  }

  protected async savePolicy(repo: Repository): Promise<void> {
    const previous = this.savedPolicies.get(repo.id) ?? repo.access_policy;
    try {
      await this.mutate(async () => {
        await this.api.patch(`/api/v1/repos/${encodeURIComponent(repo.id)}`, {
          access_policy: repo.access_policy,
        });
        this.savedPolicies.set(repo.id, repo.access_policy);
        this.feedback.showSuccess(`Access policy updated for ${repo.repo_id}.`);
        await this.load();
      }, true);
    } catch (error) {
      repo.access_policy = previous;
      this.repositories.update((items) =>
        items.map((item) => (item.id === repo.id ? { ...item, access_policy: previous } : item)),
      );
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to update this policy.'));
    }
  }

  protected async preload(repo: Repository): Promise<void> {
    if (this.preloading()[repo.id]) return;
    this.preloading.update((items) => ({ ...items, [repo.id]: true }));
    this.feedback.clear();
    try {
      await this.api.post(`/api/v1/repos/${encodeURIComponent(repo.id)}/preload`, {});
      this.feedback.showSuccess(`Preloading started for ${repo.repo_id}. Follow progress on Overview.`);
    } catch (error) {
      this.feedback.showError(AdminApiService.errorMessage(error, 'Unable to preload this repository.'));
    } finally {
      this.preloading.update((items) => ({ ...items, [repo.id]: false }));
    }
  }

  protected preloadFor(repo: Repository): PreloadJob | undefined {
    const job = this.latestPreloadFor(repo);
    return job?.status === 'queued' || job?.status === 'running' ? job : undefined;
  }

  protected isCached(repo: Repository): boolean {
    return this.latestPreloadFor(repo)?.status === 'completed';
  }

  protected preloadWasInterrupted(repo: Repository): boolean {
    return this.latestPreloadFor(repo)?.status === 'interrupted';
  }

  protected cacheIsIncomplete(repo: Repository): boolean {
    return this.latestPreloadFor(repo)?.status === 'incomplete';
  }

  private latestPreloadFor(repo: Repository): PreloadJob | undefined {
    return this.preloadJobs().find(
      (job) => job.repo_type === repo.repo_type && job.repo_id === repo.repo_id,
    );
  }

  protected async deleteRepository(repo: Repository): Promise<void> {
    if (
      !window.confirm(
        `Remove the access policy for ${repo.repo_id}? Cached artifacts will remain in storage.`,
      )
    )
      return;
    await this.mutate(async () => {
      await this.api.delete(`/api/v1/repos/${encodeURIComponent(repo.id)}`);
      await this.load();
      this.feedback.showSuccess('Repository policy removed.');
    });
  }

  protected async openGrants(repo: Repository): Promise<void> {
    this.selectedRepo.set(repo);
    this.grantDraft = { subject_type: 'user', subject_id: '', permission: 'read' };
    await this.loadGrants(repo);
  }

  protected closeGrants(): void {
    this.selectedRepo.set(null);
  }

  protected async addGrant(): Promise<void> {
    const repo = this.selectedRepo();
    const subjectID = this.grantDraft.subject_id.trim();
    if (!repo || !subjectID) return;
    await this.mutate(async () => {
      await this.api.post(`/api/v1/repos/${encodeURIComponent(repo.id)}/grants`, {
        ...this.grantDraft,
        subject_id: subjectID,
      });
      this.grantDraft.subject_id = '';
      await this.loadGrants(repo);
      this.feedback.showSuccess('Repository grant saved.');
    });
  }

  protected async removeGrant(grant: Grant): Promise<void> {
    const repo = this.selectedRepo();
    if (!repo) return;
    await this.mutate(async () => {
      const query = new URLSearchParams({
        subject_type: grant.subject_type,
        subject_id: grant.subject_id,
      });
      await this.api.delete(`/api/v1/repos/${encodeURIComponent(repo.id)}/grants?${query}`);
      await this.loadGrants(repo);
      this.feedback.showSuccess('Repository grant removed.');
    });
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

  private async loadGrants(repo: Repository): Promise<void> {
    try {
      this.grants.set(
        (
          await this.api.get<{ items: Grant[] }>(
            `/api/v1/repos/${encodeURIComponent(repo.id)}/grants`,
          )
        ).items,
      );
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, 'Unable to load repository grants.'),
      );
    }
  }

  private async loadPreloads(): Promise<void> {
    if (this.preloadPollRunning) return;
    this.preloadPollRunning = true;
    try {
      const result = await this.api.get<{ preloads: PreloadJob[] }>('/api/v1/downloads/active');
      this.preloadJobs.set(result.preloads);
    } catch {
      // Keep the most recent preload status if a poll briefly fails.
      if (!this.auth.principal() && this.preloadPoll !== undefined) {
        window.clearInterval(this.preloadPoll);
        this.preloadPoll = undefined;
      }
    } finally {
      this.preloadPollRunning = false;
    }
  }

  private async mutate(action: () => Promise<void>, propagateError = false): Promise<void> {
    this.busy.set(true);
    this.feedback.clear();
    try {
      await action();
    } catch (error) {
      if (propagateError) throw error;
      this.feedback.showError(AdminApiService.errorMessage(error, 'The repository update failed.'));
    } finally {
      this.busy.set(false);
    }
  }
}

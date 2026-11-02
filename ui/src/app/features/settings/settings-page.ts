import { Component, OnInit, inject, signal } from '@angular/core';
import { NgTemplateOutlet } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { AuthService } from '../../core/auth.service';
import { AdminApiService } from '../../core/admin-api.service';
import { FeedbackService } from '../../core/feedback.service';
import {
  CacheAdmissionPolicy,
  CacheStrategy,
  ManagedSettings,
  SettingsDraft,
} from '../../core/admin.types';
import { IconComponent } from '../../shared/icon/icon';
import { PageHeadingComponent } from '../../shared/page-heading/page-heading';

type SettingsSection =
  'connections' | 'storage' | 'access' | 'oidc' | 'ldap' | 'cache' | 'publishing' | 'observability';

@Component({
  selector: 'app-settings-page',
  standalone: true,
  imports: [FormsModule, NgTemplateOutlet, IconComponent, PageHeadingComponent],
  templateUrl: './settings-page.html',
  styleUrl: './settings-page.css',
})
export class SettingsPageComponent implements OnInit {
  private readonly api = inject(AdminApiService);
  private readonly auth = inject(AuthService);
  private readonly feedback = inject(FeedbackService);
  protected readonly loading = signal(true);
  protected readonly busy = signal(false);
  protected readonly section = signal<SettingsSection>('connections');
  protected proxyURLVisible = false;

  protected proxyToggleLabel(): string {
    return this.proxyURLVisible ? $localize`Hide proxy URL` : $localize`Show proxy URL`;
  }
  protected draft: SettingsDraft | null = null;

  protected readonly sections: { id: SettingsSection; label: string; icon: string }[] = [
    { id: 'connections', label: $localize`Connections`, icon: 'arrow' },
    { id: 'storage', label: $localize`Object storage`, icon: 'database' },
    { id: 'access', label: $localize`Access & sessions`, icon: 'lock' },
    { id: 'oidc', label: $localize`OIDC / SSO`, icon: 'key' },
    { id: 'ldap', label: $localize`LDAP`, icon: 'users' },
    { id: 'cache', label: $localize`Cache rules`, icon: 'layers' },
    { id: 'publishing', label: $localize`Publishing`, icon: 'box' },
    { id: 'observability', label: $localize`Observability`, icon: 'layers' },
  ];

  /** The eviction strategies, as the cards present them. */
  protected readonly strategies: {
    id: CacheStrategy;
    label: string;
    summary: string;
    badge?: string;
  }[] = [
    {
      id: 'lru',
      label: $localize`Least recently used`,
      summary: $localize`The file nobody asked for in the longest time goes first.`,
      badge: $localize`Default`,
    },
    {
      id: 'cost_lru',
      label: $localize`LRU weighted by cost`,
      summary: $localize`Like LRU, but a file that is slow to fetch again may stay idle longer.`,
    },
    {
      id: 'gdsf',
      label: $localize`Value per byte`,
      summary: $localize`Frequency × cost ÷ size: keeps what brings the most for the room it takes.`,
      badge: $localize`Recommended`,
    },
    {
      id: 'lfu',
      label: $localize`Least frequently used`,
      summary: $localize`Counts downloads; the count fades with time.`,
    },
    {
      id: 'fifo',
      label: $localize`First in, first out`,
      summary: $localize`A ring: the oldest cached files go, accesses do not count.`,
    },
  ];

  /** The admission policies, as the cards present them. */
  protected readonly admissions: {
    id: CacheAdmissionPolicy;
    label: string;
    summary: string;
    badge?: string;
  }[] = [
    {
      id: 'always',
      label: $localize`Store every file`,
      summary: $localize`A file is stored the first time it is asked for.`,
      badge: $localize`Default`,
    },
    {
      id: 'second_request',
      label: $localize`Store on the second request`,
      summary: $localize`The first request is served from the Hub without being kept; the file is stored when someone asks again.`,
    },
    {
      id: 'frequency',
      label: $localize`Store if asked for more than the file it would replace`,
      summary: $localize`When the cache is full, a new file must have been asked for more often than the next file to be evicted.`,
    },
  ];

  /** Draft fields that edit a setting under a different name. */
  private static readonly lockAliases: Record<string, string[]> = {
    hf_allowed_hosts_text: ['hf_allowed_hosts'],
    hf_proxy_url: ['hf_proxy_url'],
    clear_hf_proxy: ['hf_proxy_url'],
    hf_token: ['hf_token'],
    clear_hf_token: ['hf_token'],
    s3_access_key: ['s3_access_key'],
    s3_secret_key: ['s3_secret_key'],
    clear_s3_credentials: ['s3_access_key', 's3_secret_key'],
    oidc_client_secret: ['oidc_client_secret'],
    clear_oidc_client_secret: ['oidc_client_secret'],
    ldap_bind_password: ['ldap_bind_password'],
    clear_ldap_bind_password: ['ldap_bind_password'],
  };

  /** True when the environment pins this field, so the console cannot change it. */
  protected isLocked(field: string): boolean {
    const locked = this.draft?.locked ?? [];
    if (locked.includes('all')) return true;
    const names = SettingsPageComponent.lockAliases[field] ?? [field];
    return names.some((name) => locked.includes(name));
  }

  ngOnInit(): void {
    void this.load();
  }

  protected async load(): Promise<void> {
    this.loading.set(true);
    try {
      this.setDraft(await this.api.get<ManagedSettings>('/api/v1/settings'));
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to load instance settings.`),
      );
    } finally {
      this.loading.set(false);
    }
  }

  protected async save(): Promise<void> {
    const draft = this.draft;
    if (!draft) return;
    const payload: Record<string, unknown> = {
      ...draft,
      hf_allowed_hosts: draft.hf_allowed_hosts_text
        .split(',')
        .map((host) => host.trim())
        .filter(Boolean),
    };
    delete payload['hf_allowed_hosts_text'];
    delete payload['locked'];
    delete payload['hf_proxy_set'];
    delete payload['hf_token_set'];
    delete payload['s3_access_key_set'];
    delete payload['s3_secret_key_set'];
    delete payload['oidc_client_secret_set'];
    delete payload['ldap_bind_password_set'];
    if (!draft.hf_token) delete payload['hf_token'];
    if (!draft.hf_proxy_url) delete payload['hf_proxy_url'];
    if (!draft.s3_access_key) delete payload['s3_access_key'];
    if (!draft.s3_secret_key) delete payload['s3_secret_key'];
    if (!draft.oidc_client_secret) delete payload['oidc_client_secret'];
    if (!draft.ldap_bind_password) delete payload['ldap_bind_password'];
    await this.mutate(async () => {
      const saved = await this.api.put<ManagedSettings>('/api/v1/settings', payload);
      this.setDraft(saved);
      await this.auth.refreshProviders();
      this.feedback.showSuccess(
        $localize`Settings saved and applied. Secret values are write-only.`,
      );
    });
  }

  protected async testHub(): Promise<void> {
    const settings = this.draft;
    if (!settings) return;
    await this.mutate(async () => {
      await this.api.post('/api/v1/settings/test-hub', {
        hf_base_url: settings.hf_base_url,
        hf_proxy_url: settings.hf_proxy_url,
        clear_hf_proxy: settings.clear_hf_proxy,
        hf_token: settings.hf_token,
        clear_hf_token: settings.clear_hf_token,
      });
      this.feedback.showSuccess($localize`Hugging Face Hub connection succeeded.`);
    });
  }

  protected async testS3(): Promise<void> {
    const settings = this.draft;
    if (!settings) return;
    await this.mutate(async () => {
      await this.api.post('/api/v1/settings/test-s3', {
        s3_endpoint: settings.s3_endpoint,
        s3_region: settings.s3_region,
        s3_bucket: settings.s3_bucket,
        s3_prefix: settings.s3_prefix,
        s3_force_path_style: settings.s3_force_path_style,
        s3_server_side_encryption: settings.s3_server_side_encryption,
        s3_access_key: settings.s3_access_key,
        s3_secret_key: settings.s3_secret_key,
        clear_s3_credentials: settings.clear_s3_credentials,
      });
      this.feedback.showSuccess($localize`S3-compatible storage connection succeeded.`);
    });
  }

  protected async refresh(): Promise<void> {
    await this.load();
  }

  private setDraft(settings: ManagedSettings): void {
    this.draft = {
      ...settings,
      hf_allowed_hosts_text: settings.hf_allowed_hosts.join(', '),
      hf_proxy_url: '',
      clear_hf_proxy: false,
      hf_token: '',
      clear_hf_token: false,
      s3_access_key: '',
      s3_secret_key: '',
      clear_s3_credentials: false,
      oidc_client_secret: '',
      clear_oidc_client_secret: false,
      ldap_bind_password: '',
      clear_ldap_bind_password: false,
    };
  }

  private async mutate(action: () => Promise<void>): Promise<void> {
    this.busy.set(true);
    this.feedback.clear();
    try {
      await action();
    } catch (error) {
      this.feedback.showError(
        AdminApiService.errorMessage(error, $localize`Unable to apply these settings.`),
      );
    } finally {
      this.busy.set(false);
    }
  }
}

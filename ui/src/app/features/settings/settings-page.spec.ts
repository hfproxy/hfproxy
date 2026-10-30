import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ManagedSettings } from '../../core/admin.types';
import { SettingsPageComponent } from './settings-page';

const settings: ManagedSettings = {
  revision: 'r1',
  public_url: 'https://hf.example.com',
  hf_base_url: 'https://huggingface.co',
  hf_proxy_set: false,
  hf_allowed_hosts: ['huggingface.co'],
  hf_timeout: '2m0s',
  hf_token_set: true,
  s3_endpoint: '',
  s3_region: 'us-east-1',
  s3_bucket: 'bucket',
  s3_prefix: 'hfproxy',
  s3_force_path_style: true,
  s3_server_side_encryption: true,
  s3_access_key_set: true,
  s3_secret_key_set: true,
  auth_mode: 'local',
  anonymous_public_read: false,
  session_idle_ttl: '8h0m0s',
  session_absolute_ttl: '24h0m0s',
  cache_max_file_size: 107374182400,
  cache_eviction: {
    strategy: 'lru',
    target_percent: 90,
    max_repository_size: 0,
    max_namespace_size: 0,
    max_revisions: 0,
    max_idle_hours: 0,
    fifo: { ring_size: 0, ring_files: 0 },
    frequency: { half_life_hours: 168 },
    cost: { weight: 0.5, bandwidth: 52428800, latency_seconds: 1 },
    admission: { policy: 'always', window_hours: 168, min_size: 67108864 },
  },
  cache_max_total_size: 5368709120,
  cache_ref_ttl: '1m0s',
  cache_negative_ttl: '30s',
  cache_gc_grace: '24h0m0s',
  download_max_concurrent: 0,
  download_rate_limit: 0,
  download_daily_quota: 0,
  custom_repositories: true,
  safe_formats_only: false,
  upload_max_file_size: 53687091200,
  upload_user_quota: 0,
  oidc_issuer: '',
  oidc_client_id: '',
  oidc_client_secret_set: false,
  oidc_redirect_url: '',
  oidc_auto_provision: false,
  ldap_url: '',
  ldap_starttls: true,
  ldap_bind_dn: '',
  ldap_bind_password_set: false,
  ldap_base_dn: '',
  ldap_user_filter: '(uid={username})',
  ldap_auto_provision: false,
  metrics_enabled: false,
  metrics_path: '/metrics',
  tracing_enabled: false,
  tracing_service_name: 'hfproxy',
  tracing_sampling_ratio: 0.1,
  otlp_endpoint: '',
  cache_inventory_refresh: '5m0s',
  log_level: 'info',
  locked: [],
} as ManagedSettings;

async function flushMicrotasks(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

interface SettingsPageInternals {
  draft: (ManagedSettings & Record<string, unknown>) | null;
  save(): Promise<void>;
}

describe('Settings page', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
  });

  it('shows the total cache size limit and sends a changed value back to the server', async () => {
    const page = TestBed.runInInjectionContext(() => new SettingsPageComponent());
    const internals = page as unknown as SettingsPageInternals;
    page.ngOnInit();
    http.expectOne('/api/v1/settings').flush(settings);
    await flushMicrotasks();

    expect(internals.draft?.cache_max_total_size).toBe(5368709120);

    internals.draft!.cache_max_total_size = 107374182400;
    const saving = internals.save();
    await flushMicrotasks();
    const put = http.expectOne(
      (request) => request.method === 'PUT' && request.url === '/api/v1/settings',
    );
    const body = put.request.body as Record<string, unknown>;
    expect(body['cache_max_total_size']).toBe(107374182400);
    // Read-only flags and the draft helpers never go back to the server.
    for (const key of Object.keys(body)) expect(key.endsWith('_set')).toBe(false);
    expect(body['hf_allowed_hosts_text']).toBeUndefined();
    put.flush({ ...settings, cache_max_total_size: 107374182400 });
    await flushMicrotasks();
    http
      .match('/api/v1/auth/providers')
      .forEach((request) => request.flush({ local: true, oidc: false, ldap: false }));
    await saving;
    expect(internals.draft?.cache_max_total_size).toBe(107374182400);
  });

  it('keeps 0 as "no limit" instead of dropping it from the request', async () => {
    const page = TestBed.runInInjectionContext(() => new SettingsPageComponent());
    const internals = page as unknown as SettingsPageInternals;
    page.ngOnInit();
    http.expectOne('/api/v1/settings').flush(settings);
    await flushMicrotasks();

    internals.draft!.cache_max_total_size = 0;
    const saving = internals.save();
    await flushMicrotasks();
    const put = http.expectOne((request) => request.method === 'PUT');
    expect((put.request.body as Record<string, unknown>)['cache_max_total_size']).toBe(0);
    put.flush({ ...settings, cache_max_total_size: 0 });
    await flushMicrotasks();
    http
      .match('/api/v1/auth/providers')
      .forEach((request) => request.flush({ local: true, oidc: false, ldap: false }));
    await saving;
  });

  it('renders the limit as an editable number field in the cache rules', async () => {
    const fixture = TestBed.createComponent(SettingsPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/settings').flush(settings);
    await flushMicrotasks();
    const internals = fixture.componentInstance as unknown as {
      section: { set(value: string): void };
    };
    internals.section.set('cache');
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();

    const field = fixture.nativeElement.querySelector(
      'input[name="maxTotalSize"]',
    ) as HTMLInputElement | null;
    expect(field).not.toBeNull();
    expect(field!.type).toBe('number');
    expect(field!.min).toBe('0');
    expect(field!.value).toBe('5368709120');
    expect(fixture.nativeElement.textContent).toContain('are never removed');
  });

  it('shows the settings of the chosen strategy and sends the eviction settings back', async () => {
    const fixture = TestBed.createComponent(SettingsPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/settings').flush(settings);
    await flushMicrotasks();
    const internals = fixture.componentInstance as unknown as {
      section: { set(value: string): void };
      draft: ManagedSettings | null;
      save(): Promise<void>;
    };
    internals.section.set('cache');
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    const root = fixture.nativeElement as HTMLElement;
    expect(root.querySelectorAll('input[name="evictionStrategy"]').length).toBe(5);
    expect(root.querySelector('input[name="ringFiles"]')).toBeNull();
    expect(root.querySelector('input[name="costWeight"]')).toBeNull();
    expect(root.querySelectorAll('input[name="admissionPolicy"]').length).toBe(3);
    expect(root.querySelector('input[name="admissionWindowHours"]')).toBeNull();
    (root.querySelector('#admission-second_request') as HTMLInputElement).click();
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    expect(root.querySelector('input[name="admissionWindowHours"]')).not.toBeNull();
    expect(root.textContent).toContain('Right for a shared proxy where many models are tried once');

    (root.querySelector('#strategy-fifo') as HTMLInputElement).click();
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    expect(internals.draft!.cache_eviction.strategy).toBe('fifo');
    const ring = root.querySelector('input[name="ringFiles"]') as HTMLInputElement | null;
    expect(ring).not.toBeNull();
    ring!.value = '1000';
    ring!.dispatchEvent(new Event('input'));
    fixture.detectChanges();
    await fixture.whenStable();
    expect(root.textContent).toContain('A ring: files leave in the order they came in');

    const saving = internals.save();
    await flushMicrotasks();
    const put = http.expectOne((request) => request.method === 'PUT');
    const body = put.request.body as Record<string, unknown>;
    expect(body['cache_eviction']).toEqual(
      expect.objectContaining({
        strategy: 'fifo',
        fifo: { ring_size: 0, ring_files: 1000 },
        admission: expect.objectContaining({ policy: 'second_request' }),
      }),
    );
    put.flush({ ...settings, cache_eviction: body['cache_eviction'] });
    await flushMicrotasks();
    http
      .match('/api/v1/auth/providers')
      .forEach((request) => request.flush({ local: true, oidc: false, ldap: false }));
    await saving;
  });

  it('lets the log level be chosen in the observability section and sends it back', async () => {
    const fixture = TestBed.createComponent(SettingsPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/settings').flush(settings);
    await flushMicrotasks();
    const internals = fixture.componentInstance as unknown as {
      section: { set(value: string): void };
      draft: ManagedSettings | null;
      save(): Promise<void>;
    };
    internals.section.set('observability');
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();

    const select = fixture.nativeElement.querySelector(
      'select[name="logLevel"]',
    ) as HTMLSelectElement | null;
    expect(select).not.toBeNull();
    expect([...select!.options].map((option) => option.value)).toEqual([
      'debug',
      'info',
      'warn',
      'error',
    ]);
    expect(select!.value).toBe('info');
    expect(select!.disabled).toBe(false);

    select!.value = 'debug';
    select!.dispatchEvent(new Event('change'));
    expect(internals.draft?.log_level).toBe('debug');
    const saving = internals.save();
    await flushMicrotasks();
    const put = http.expectOne((request) => request.method === 'PUT');
    expect((put.request.body as Record<string, unknown>)['log_level']).toBe('debug');
    put.flush({ ...settings, log_level: 'debug' });
    await flushMicrotasks();
    http
      .match('/api/v1/auth/providers')
      .forEach((request) => request.flush({ local: true, oidc: false, ldap: false }));
    await saving;
  });

  it('disables the log level when the environment locks it', async () => {
    const fixture = TestBed.createComponent(SettingsPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/settings').flush({ ...settings, locked: ['log_level'] });
    await flushMicrotasks();
    (fixture.componentInstance as unknown as { section: { set(value: string): void } }).section.set(
      'observability',
    );
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    const select = fixture.nativeElement.querySelector(
      'select[name="logLevel"]',
    ) as HTMLSelectElement;
    expect(select.disabled).toBe(true);
  });

  it('knows which fields the environment locks and never sends the list back', async () => {
    const page = TestBed.runInInjectionContext(() => new SettingsPageComponent());
    const internals = page as unknown as SettingsPageInternals & {
      isLocked(field: string): boolean;
    };
    page.ngOnInit();
    http
      .expectOne('/api/v1/settings')
      .flush({ ...settings, locked: ['cache_max_total_size', 's3_access_key'] });
    await flushMicrotasks();

    expect(internals.isLocked('cache_max_total_size')).toBe(true);
    expect(internals.isLocked('s3_access_key')).toBe(true);
    expect(internals.isLocked('cache_ref_ttl')).toBe(false);
    expect(internals.isLocked('clear_s3_credentials')).toBe(true); // clears the locked access key
    expect(internals.isLocked('s3_secret_key')).toBe(false);

    const saving = internals.save();
    await flushMicrotasks();
    const put = http.expectOne(
      (request) => request.method === 'PUT' && request.url === '/api/v1/settings',
    );
    expect((put.request.body as Record<string, unknown>)['locked']).toBeUndefined();
    put.flush({ ...settings, locked: [] });
    await flushMicrotasks();
    http
      .match('/api/v1/auth/providers')
      .forEach((request) => request.flush({ local: true, oidc: false, ldap: false }));
    await saving;
  });
});

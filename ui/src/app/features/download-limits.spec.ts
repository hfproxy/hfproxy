import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { AuthService } from '../core/auth.service';
import { ManagedSettings } from '../core/admin.types';
import { SettingsPageComponent } from './settings/settings-page';
import { UsersPageComponent } from './users/users-page';

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

const settings = {
  public_url: 'https://hf.example.com',
  hf_base_url: 'https://huggingface.co',
  hf_allowed_hosts: ['huggingface.co'],
  hf_timeout: '2m0s',
  auth_mode: 'local',
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
  cache_max_total_size: 0,
  download_max_concurrent: 0,
  download_rate_limit: 0,
  download_daily_quota: 0,
  custom_repositories: true,
  upload_max_file_size: 53687091200,
  upload_user_quota: 0,
  locked: [] as string[],
} as unknown as ManagedSettings;

describe('Download limits in the settings', () => {
  let http: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
  });

  async function openCacheRules(locked: string[] = []) {
    const fixture = TestBed.createComponent(SettingsPageComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/settings').flush({ ...settings, locked });
    await flush();
    (fixture.componentInstance as unknown as { section: { set(value: string): void } }).section.set(
      'cache',
    );
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    return fixture;
  }

  it('shows the three limits as numbers and sends them back', async () => {
    const fixture = await openCacheRules();
    for (const name of ['downloadMaxConcurrent', 'downloadRateLimit', 'downloadDailyQuota']) {
      const input = fixture.nativeElement.querySelector(
        `input[name="${name}"]`,
      ) as HTMLInputElement | null;
      expect(input, name).not.toBeNull();
      expect(input!.type).toBe('number');
      expect(input!.min).toBe('0');
      expect(input!.disabled).toBe(false);
    }
    const internals = fixture.componentInstance as unknown as {
      draft: ManagedSettings;
      save(): Promise<void>;
    };
    internals.draft.download_daily_quota = 5368709120;
    internals.draft.download_max_concurrent = 3;
    const saving = internals.save();
    await flush();
    const put = http.expectOne((request) => request.method === 'PUT');
    const body = put.request.body as Record<string, unknown>;
    expect(body['download_daily_quota']).toBe(5368709120);
    expect(body['download_max_concurrent']).toBe(3);
    expect(body['download_rate_limit']).toBe(0);
    put.flush({ ...settings, download_daily_quota: 5368709120 });
    await flush();
    http
      .match('/api/v1/auth/providers')
      .forEach((request) => request.flush({ local: true, oidc: false, ldap: false }));
    await saving;
  });

  it('disables a limit that the environment locks', async () => {
    const fixture = await openCacheRules(['download_rate_limit']);
    const locked = fixture.nativeElement.querySelector(
      'input[name="downloadRateLimit"]',
    ) as HTMLInputElement;
    const free = fixture.nativeElement.querySelector(
      'input[name="downloadDailyQuota"]',
    ) as HTMLInputElement;
    expect(locked.disabled).toBe(true);
    expect(free.disabled).toBe(false);
  });
});

describe('User quotas', () => {
  let http: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
    TestBed.inject(AuthService).principal.set({
      id: 'admin',
      username: 'root',
      role: 'admin',
      authenticated: true,
    });
  });

  interface UsersInternals {
    users: () => { id: string; role: string; status: string; daily_quota_bytes?: number | null }[];
    quotaText(user: unknown): string;
    setQuota(user: unknown, text: string | number | null): void;
    save(user: unknown): Promise<void>;
  }

  async function loaded(users: unknown[]) {
    const page = TestBed.runInInjectionContext(() => new UsersPageComponent());
    page.ngOnInit();
    await flush();
    http
      .expectOne((request) => request.url.startsWith('/api/v1/users'))
      .flush({ items: users, next_cursor: '' });
    await flush();
    return page as unknown as UsersInternals;
  }

  const user = (id: string, quota: number | null | undefined) => ({
    id,
    username: id,
    email: '',
    display_name: '',
    role: 'reader',
    status: 'active',
    created_at: '',
    daily_quota_bytes: quota,
  });

  it('shows the quota in GiB and leaves the default empty', async () => {
    const page = await loaded([
      user('a', null),
      user('b', 0),
      user('c', 5 * 2 ** 30),
      user('d', 1.5 * 2 ** 30),
    ]);
    const [a, b, c, d] = page.users();
    expect(page.quotaText(a)).toBe('');
    expect(page.quotaText(b)).toBe('0');
    expect(page.quotaText(c)).toBe('5');
    expect(page.quotaText(d)).toBe('1.5');
  });

  it('only sends the quota when it was edited, converted to bytes', async () => {
    const page = await loaded([user('a', null), user('b', 3 * 2 ** 30)]);
    const [a, b] = page.users();

    let saving = page.save(a);
    await flush();
    let patch = http.expectOne(
      (request) => request.method === 'PATCH' && request.url === '/api/v1/users/a',
    );
    expect(Object.keys(patch.request.body as object)).not.toContain('daily_quota_bytes');
    patch.flush({});
    await saving;

    page.setQuota(a, 2);
    saving = page.save(a);
    await flush();
    patch = http.expectOne('/api/v1/users/a');
    expect((patch.request.body as Record<string, unknown>)['daily_quota_bytes']).toBe(2 * 2 ** 30);
    patch.flush({});
    await saving;

    page.setQuota(b, ''); // back to the default
    saving = page.save(b);
    await flush();
    patch = http.expectOne('/api/v1/users/b');
    expect((patch.request.body as Record<string, unknown>)['daily_quota_bytes']).toBeNull();
    patch.flush({});
    await saving;
  });

  it('refuses a quota that is not a number', async () => {
    const page = await loaded([user('a', null)]);
    page.setQuota(page.users()[0], 'lots');
    await page.save(page.users()[0]);
    http.expectNone((request) => request.method === 'PATCH');
  });
});

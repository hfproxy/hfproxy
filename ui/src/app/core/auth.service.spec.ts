import { TestBed } from '@angular/core/testing';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { AuthService } from './auth.service';

describe('AuthService session handling', () => {
  let auth: AuthService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    auth = TestBed.inject(AuthService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('starts without an authenticated principal', () => {
    expect(auth.principal()).toBeNull();
    expect(auth.busy()).toBeFalsy();
  });

  it('clears the principal when the session expires', () => {
    auth.principal.set({ id: 'u1', username: 'admin', role: 'admin', authenticated: true });
    auth.expireSession();
    expect(auth.principal()).toBeNull();
    expect(auth.error()).toContain('expired');
  });

  it('keeps the principal and reports a failed sign-out', async () => {
    auth.principal.set({ id: 'u1', username: 'admin', role: 'admin', authenticated: true });
    const pending = auth.signOut();
    http.expectOne('/logout').flush({ detail: 'CSRF validation failed' }, {
      status: 403,
      statusText: 'Forbidden',
    });
    await pending;
    expect(auth.principal()?.id).toBe('u1');
    expect(auth.error()).toContain('CSRF');
  });
});

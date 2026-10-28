import { inject } from '@angular/core';
import { CanActivateFn, Router, Routes } from '@angular/router';
import { AuthService } from './core/auth.service';
import { AuditPageComponent } from './features/audit/audit-page';
import { CachePageComponent } from './features/cache/cache-page';
import { GroupsPageComponent } from './features/groups/groups-page';
import { OverviewPageComponent } from './features/overview/overview-page';
import { RepositoriesPageComponent } from './features/repositories/repositories-page';
import { SettingsPageComponent } from './features/settings/settings-page';
import { UsersPageComponent } from './features/users/users-page';
import { TokensPageComponent } from './features/tokens/tokens-page';
import { AdminShellComponent } from './layout/admin-shell';

const adminOnly: CanActivateFn = async () => {
  const auth = inject(AuthService);
  const router = inject(Router);
  await auth.initialize();
  return auth.principal()?.role === 'admin' ? true : router.parseUrl('/overview');
};

export const routes: Routes = [
  {
    path: '',
    component: AdminShellComponent,
    children: [
      { path: '', pathMatch: 'full', redirectTo: 'overview' },
      { path: 'overview', component: OverviewPageComponent },
      { path: 'tokens', component: TokensPageComponent },
      { path: 'repositories', component: RepositoriesPageComponent, canActivate: [adminOnly] },
      { path: 'cache', component: CachePageComponent, canActivate: [adminOnly] },
      { path: 'users', component: UsersPageComponent, canActivate: [adminOnly] },
      { path: 'groups', component: GroupsPageComponent, canActivate: [adminOnly] },
      { path: 'audit', component: AuditPageComponent, canActivate: [adminOnly] },
      { path: 'settings', component: SettingsPageComponent, canActivate: [adminOnly] },
    ],
  },
  { path: '**', redirectTo: 'overview' },
];

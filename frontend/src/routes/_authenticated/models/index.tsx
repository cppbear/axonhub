import { useEffect, useState } from 'react';
import { createFileRoute, useRouter } from '@tanstack/react-router';
import { z } from 'zod';
import { usePermissions } from '@/hooks/usePermissions';
import { RouteGuard } from '@/components/route-guard';
import ModelsManagement from '@/features/models';
import { ModelsCatalogImport } from '@/features/models/components/models-catalog-import';

const reviewScope = z.object({
  channels: z.boolean(),
  providers: z.array(z.string().trim().min(1)),
  ignorePrefix: z.boolean(),
});

function ProtectedModels() {
  const { catalogReview } = Route.useSearch();
  const navigate = Route.useNavigate();
  const router = useRouter();
  const { hasSystemScope } = usePermissions();
  const canRead = hasSystemScope('read_channels');
  const canReview = hasSystemScope('write_channels');
  const [savedScope, setSavedScope] = useState<typeof catalogReview>();
  const scope = catalogReview ?? savedScope;

  useEffect(() => {
    if (catalogReview && canReview) {
      setSavedScope(catalogReview);
      void navigate({ search: {}, replace: true });
    }
  }, [catalogReview, canReview, navigate]);

  useEffect(() => {
    return router.history.subscribe(({ action }) => {
      // Replacing the review URL consumes its scope within the same history entry.
      if (action.type !== 'REPLACE') setSavedScope(undefined);
    });
  }, [router]);

  function closeReview() {
    setSavedScope(undefined);
    if (canRead) return;
    if (router.history.canGoBack()) router.history.back();
    else void navigate({ to: '/settings/profile', search: { oidc_link: undefined }, replace: true });
  }

  return (
    <RouteGuard requiredScopes={[scope ? 'write_channels' : 'read_channels']} scopeLevel='system'>
      {canRead && <ModelsManagement />}
      {scope && <ModelsCatalogImport reviewScope={{ ...scope, review: true }} onClose={closeReview} />}
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/models/')({
  component: ProtectedModels,
  validateSearch: (search: Record<string, unknown>): { catalogReview?: z.infer<typeof reviewScope> } => {
    const scope = reviewScope.safeParse(search.catalogReview);
    return scope.success ? { catalogReview: scope.data } : {};
  },
});

import { useEffect, useState } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { usePermissions } from '@/hooks/usePermissions';
import type { ImportScope } from '../data/catalog-import';
import { ModelsCatalogImport } from './models-catalog-import';
import { useModels } from '../context/models-context';
import { ModelsActionDialog } from './models-action-dialog';
import { ModelsArchiveDialog } from './models-archive-dialog';
import { ModelsAssociationDialog } from './models-association-dialog';
import { ModelsBatchCreateDialog } from './models-batch-create-dialog';
import { ModelsBulkDisableDialog } from './models-bulk-disable-dialog';
import { ModelsBulkEnableDialog } from './models-bulk-enable-dialog';
import { ModelsDeleteDialog } from './models-delete-dialog';
import { ModelSettingsDialog } from './models-settings-dialog';
import { ModelsUnassociatedDialog } from './models-unassociated-dialog';

export function ModelsDialogs() {
  const { open, setOpen } = useModels();
  const { catalogReview } = useSearch({ from: '/_authenticated/models/' });
  const navigate = useNavigate({ from: '/models/' });
  const { hasSystemScope } = usePermissions();
  const canReview = hasSystemScope('write_channels');
  const [reviewScope, setReviewScope] = useState<ImportScope>();

  useEffect(() => {
    if (!catalogReview || !canReview) return;
    setReviewScope({ ...catalogReview, review: true });
    setOpen('catalogImport');
    void navigate({ search: {}, replace: true });
  }, [catalogReview, canReview, navigate, setOpen]);

  return (
    <>
      {(open === 'create' || open === 'edit') && <ModelsActionDialog />}
      {open === 'batchCreate' && <ModelsBatchCreateDialog />}
      {open === 'catalogImport' && canReview && (
        <ModelsCatalogImport
          reviewScope={reviewScope}
          onClose={() => {
            setReviewScope(undefined);
            setOpen(null);
          }}
        />
      )}
      {open === 'delete' && <ModelsDeleteDialog />}
      {open === 'archive' && <ModelsArchiveDialog />}
      {(open === 'association' || open === 'developerAssociation') && <ModelsAssociationDialog />}
      {open === 'settings' && <ModelSettingsDialog />}
      {open === 'unassociated' && <ModelsUnassociatedDialog />}
      <ModelsBulkDisableDialog />
      <ModelsBulkEnableDialog />
    </>
  );
}

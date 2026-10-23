create index if not exists generated_documents_job_kind_idx
    on generated_documents(job_id, document_type, id desc);
create index if not exists documentation_jobs_project_created_idx
    on documentation_jobs(project_id, created_at desc, id desc);

import React from "react";

const baseClass = "empty-users";

const EmptyUsers = (): JSX.Element => {
  return (
    <div className={`${baseClass}`}>
      <div className={`${baseClass}__inner`}>
        <div className={`${baseClass}__empty-filter-results`}>
          <h1>No users matched your search criteria.</h1>
          <p>Try a different search.</p>
        </div>
      </div>
    </div>
  );
};

export default EmptyUsers;
